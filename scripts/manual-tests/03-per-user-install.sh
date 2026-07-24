#!/usr/bin/env bash
# Test 3 — per-user LaunchAgent install & log location (H2 fix).
#
# WARNING: this MUTATES your system — it runs ./install.sh, which copies the
# binary to /usr/local/bin (sudo) and installs a LaunchAgent under
# ~/Library/LaunchAgents. The optional load step starts the agent.
#
#   ./scripts/manual-tests/03-per-user-install.sh [printer-ip]
#   PRINTER_IP=192.168.1.128 ./scripts/manual-tests/03-per-user-install.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PLIST_DEST="$HOME/Library/LaunchAgents/com.local.samsung-scan.plist"
USER_LOG="$HOME/Library/Logs/samsung-scan.log"
LOADED=0
cleanup() { [ "$LOADED" -eq 1 ] && launchctl unload "$PLIST_DEST" 2>/dev/null; }
trap cleanup EXIT

rc=0
info "Test 3 — per-user install & log location"
warn "This runs ./install.sh and will use sudo for the binary copy."
confirm "Proceed with the install?" || { warn "skipped"; exit 0; }

step "Running install.sh"
if ( cd "$REPO_ROOT" && ./install.sh ); then pass "install.sh completed"; else fail "install.sh failed"; exit 1; fi

# --- static checks on the installed plist ----------------------------------
step "Checking installed plist"
if [ -f "$PLIST_DEST" ]; then pass "plist installed at $PLIST_DEST"; else fail "plist not found at $PLIST_DEST"; exit 1; fi

if grep -q '__HOME__' "$PLIST_DEST"; then
  fail "plist still contains __HOME__ placeholder (install.sh substitution failed)"; rc=1
else
  pass "no __HOME__ placeholder left"
fi

out_path="$(plutil -extract StandardOutPath raw "$PLIST_DEST" 2>/dev/null)"
if [ "$out_path" = "$USER_LOG" ]; then
  pass "StandardOutPath = $out_path"
else
  fail "StandardOutPath is '$out_path', expected '$USER_LOG'"; rc=1
fi

if [ -d "$HOME/Library/Logs" ]; then pass "~/Library/Logs exists"; else fail "~/Library/Logs missing"; rc=1; fi

# --- optional: load the agent and verify runtime posture --------------------
if confirm "Load the agent and verify it runs as you, logging to ~/Library/Logs?"; then
  require_ip "${1:-}"
  step "Setting printer IP in the installed plist"
  sed -i '' "s|192\.168\.1\.128|$PRINTER_IP|" "$PLIST_DEST" 2>/dev/null || \
    warn "could not sed the IP — the plist may already use a different address"

  # Note where /tmp log stood before, to detect any fresh root-style writes.
  tmp_before="(absent)"; [ -e /tmp/samsung-scan.log ] && tmp_before="$(stat -f %m /tmp/samsung-scan.log)"

  step "launchctl load $PLIST_DEST"
  launchctl unload "$PLIST_DEST" 2>/dev/null || true
  if launchctl load "$PLIST_DEST"; then LOADED=1; pass "agent loaded"; else fail "launchctl load failed"; exit 1; fi

  sleep 3
  pid="$(pgrep -f 'samsung-scan --ip' | head -n1)"
  if [ -n "$pid" ]; then
    run_user="$(ps -o user= -p "$pid" | tr -d ' ')"
    if [ "$run_user" = "$(whoami)" ]; then pass "running as $run_user (not root)"; else fail "running as $run_user, expected $(whoami)"; rc=1; fi
  else
    warn "could not find the running process (it may have exited if the IP is wrong)"
  fi

  if wait_for_log "$USER_LOG" '.' 10 && [ -s "$USER_LOG" ]; then
    pass "per-user log is being written: $USER_LOG"
  else
    fail "nothing written to $USER_LOG"; rc=1
  fi

  tmp_after="(absent)"; [ -e /tmp/samsung-scan.log ] && tmp_after="$(stat -f %m /tmp/samsung-scan.log)"
  if [ "$tmp_before" = "$tmp_after" ]; then
    pass "/tmp/samsung-scan.log not written by this run (before=$tmp_before after=$tmp_after)"
  else
    fail "/tmp/samsung-scan.log was modified — log did not move off /tmp"; rc=1
  fi

  step "Unloading the agent"
  launchctl unload "$PLIST_DEST" 2>/dev/null && LOADED=0 && pass "agent unloaded"
else
  info "Skipped the load step. To load it yourself later:"
  info "  \$EDITOR $PLIST_DEST   # set your printer IP"
  info "  launchctl load $PLIST_DEST"
fi

if [ "$rc" -eq 0 ]; then info "${_G}Test 3 passed.${_Z}"; else info "${_R}Test 3 failed.${_Z}"; fi
exit "$rc"
