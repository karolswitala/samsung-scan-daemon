#!/usr/bin/env bash
# Runs the manual test suite in order.
#   00 unit pre-flight (automated)
#   01 normal scan            (needs printer + a scan from the LCD)
#   02 power-cycle recovery   (needs printer + you to power-cycle it)
#
# 01 and 02 must be run from a local Terminal, not over SSH: the daemon only
# registers for the active console user (see isActiveConsoleUser in main.go).
#
#   ./scripts/manual-tests/run-all.sh <printer-ip>
#   PRINTER_IP=192.168.1.128 ./scripts/manual-tests/run-all.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
DIR="$(dirname "${BASH_SOURCE[0]}")"

require_ip "${1:-}"
export PRINTER_IP

overall=0
for t in 00-unit 01-normal-scan 02-power-cycle-recovery; do
  info "==================== $t ===================="
  if confirm "Run $t?"; then
    bash "$DIR/$t.sh" "$PRINTER_IP" || { overall=1; confirm "Continue with the remaining tests?" || break; }
  else
    warn "skipped $t"
  fi
done

if [ "$overall" -eq 0 ]; then info "${_G}All executed tests passed.${_Z}"; else info "${_R}One or more tests failed — see output above.${_Z}"; fi
exit "$overall"
