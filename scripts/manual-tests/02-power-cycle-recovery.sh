#!/usr/bin/env bash
# Test 2 — power-cycle recovery against real hardware.
# Verifies the daemon detects the printer dropping our registration and
# re-registers automatically when it comes back.
#
# The same path is covered hermetically by TestRunReRegistersAfterPowerCycle in
# cmd/samsung-scan/recovery_test.go, which also asserts the two log literals this
# script greps for. This script is the real-printer confirmation on top of that.
#
# Run from a local Terminal, not over SSH — the daemon only registers for the
# active console user.
#
# Uses --poll 1s so the 3-failure threshold trips in ~3s instead of ~9s.
#
#   ./scripts/manual-tests/02-power-cycle-recovery.sh <printer-ip>
#   PRINTER_IP=192.168.1.128 ./scripts/manual-tests/02-power-cycle-recovery.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_ip "${1:-}"
ensure_binary
kill_running

LOG="$(mktemp -t samsung-scan-test2.XXXXXX)"
DAEMON_PID=""
cleanup() {
  [ -n "$DAEMON_PID" ] && kill "$DAEMON_PID" 2>/dev/null
  [ -n "$DAEMON_PID" ] && wait "$DAEMON_PID" 2>/dev/null
  rm -f "$LOG"
}
trap cleanup EXIT

rc=0
info "Test 2 — power-cycle recovery"
step "Starting daemon with --poll 1s (log: $LOG)"
"$BINARY" --ip "$PRINTER_IP" --poll 1s --log-level debug >"$LOG" 2>&1 &
DAEMON_PID=$!

if wait_for_log "$LOG" 'msg=registered ' 30; then
  first_id="$(grep -m1 'msg=registered ' "$LOG" | sed -n 's/.*instanceID=\([0-9]*\).*/\1/p')"
  pass "initial registration (instanceID=$first_id)"
else
  fail "did not register within 30s — printer reachable at $PRINTER_IP?"
  tail -n 20 "$LOG"; exit 1
fi

info "STEP 1 — Power OFF the printer now (or unplug its network)."
press_enter "Press Enter once it is powered off..."

step "Watching for poll failures (consecutive count should climb)..."
if wait_for_log "$LOG" 'consecutive=3' 30; then
  pass "daemon detected the outage (SNMP poll error, consecutive=3)"
else
  fail "no rising poll-failure count seen — did the printer actually go offline?"
  tail -n 20 "$LOG"; rc=1
fi

info "STEP 2 — Power the printer back ON now."
press_enter "Press Enter once it is booting / back on the network..."

step "Waiting up to 180s for automatic re-registration..."
if wait_for_log "$LOG" 'msg="re-registered after outage"' 180; then
  new_id="$(grep -m1 'msg="re-registered after outage"' "$LOG" | sed -n 's/.*instanceID=\([0-9]*\).*/\1/p')"
  pass "re-registered after outage (new instanceID=$new_id)"
  [ -n "$first_id" ] && [ "$new_id" = "$first_id" ] && warn "instanceID unchanged ($new_id) — usually it changes, but not always"
else
  fail "no automatic re-registration within 180s of the printer returning"
  tail -n 30 "$LOG"; rc=1
fi

info "MANUAL CHECK — confirm 'My Mac' is back in the printer's Scan → PC menu,"
info "and optionally run a scan to confirm it downloads with the new registration."

if [ "$rc" -eq 0 ]; then info "${_G}Test 2 passed (log path verified — do the manual menu/scan check above).${_Z}"
else info "${_R}Test 2 failed.${_Z}"; fi
exit "$rc"
