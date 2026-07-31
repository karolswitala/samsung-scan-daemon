#!/usr/bin/env bash
# Test 1 — normal operation still works (regression check).
# Registers with the printer, then waits for you to scan a page from the LCD and
# verifies the file lands on your ~/Desktop (the per-user default output).
#
# Run from a local Terminal, not over SSH: the daemon registers only for the
# active console user, so under SSH it stays idle and this test times out.
#
#   ./scripts/manual-tests/01-normal-scan.sh <printer-ip>
#   PRINTER_IP=192.168.1.128 ./scripts/manual-tests/01-normal-scan.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_ip "${1:-}"
ensure_binary
kill_running

LOG="$(mktemp -t samsung-scan-test1.XXXXXX)"
DAEMON_PID=""
cleanup() {
  [ -n "$DAEMON_PID" ] && kill "$DAEMON_PID" 2>/dev/null
  [ -n "$DAEMON_PID" ] && wait "$DAEMON_PID" 2>/dev/null
  rm -f "$LOG"
}
trap cleanup EXIT

rc=0
info "Test 1 — normal scan (output should land on $HOME/Desktop)"
step "Starting daemon (log: $LOG)"
"$BINARY" --ip "$PRINTER_IP" --log-level debug >"$LOG" 2>&1 &
DAEMON_PID=$!

if wait_for_log "$LOG" 'msg=registered ' 30; then
  pass "registered with printer"
else
  fail "did not register within 30s — is the printer on and reachable at $PRINTER_IP?"
  echo "--- last log lines ---"; tail -n 20 "$LOG"
  exit 1
fi

info "Now on the printer: Scan → PC → My Mac, then start the scan."
step "Waiting up to 180s for a completed scan..."
if wait_for_log "$LOG" 'msg="scan saved"' 180; then
  saved_path="$(grep -m1 'msg="scan saved"' "$LOG" | sed -n 's/.*path=\([^ ]*\).*/\1/p')"
  pass "scan saved: $saved_path"
  if [ -f "$saved_path" ]; then
    pass "output file exists on disk"
  else
    fail "log reported a path but the file is missing"; rc=1
  fi
  case "$saved_path" in
    "$HOME/Desktop/"*) pass "written to your ~/Desktop (per-user default)";;
    *) warn "not under ~/Desktop — expected the per-user default (was --output set?)";;
  esac
else
  fail "no scan completed within 180s"; echo "--- last log lines ---"; tail -n 20 "$LOG"; rc=1
fi

if [ "$rc" -eq 0 ]; then info "${_G}Test 1 passed.${_Z}"; else info "${_R}Test 1 failed.${_Z}"; fi
exit "$rc"
