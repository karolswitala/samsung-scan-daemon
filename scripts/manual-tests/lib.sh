#!/usr/bin/env bash
# Shared helpers for the samsung-scan manual test scripts.
# Sourced by the numbered test scripts in this directory.

set -uo pipefail

# Repo root is two levels up from this file (scripts/manual-tests/lib.sh).
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BINARY="$REPO_ROOT/dist/samsung-scan-macos"

# Make `go`/`make build-mac` work even when Go isn't on the login PATH.
export PATH="$PATH:/usr/local/go/bin"

# --- output helpers ---------------------------------------------------------
if [ -t 1 ]; then
  _B=$(tput bold); _R=$(tput setaf 1); _G=$(tput setaf 2); _Y=$(tput setaf 3); _Z=$(tput sgr0)
else
  _B=; _R=; _G=; _Y=; _Z=
fi
info()  { printf '\n%s\n' "${_B}$*${_Z}"; }
step()  { printf '%s\n' "${_B}➜ $*${_Z}"; }
pass()  { printf '%s\n' "${_G}✓ PASS:${_Z} $*"; }
fail()  { printf '%s\n' "${_R}✗ FAIL:${_Z} $*"; }
warn()  { printf '%s\n' "${_Y}! ${_Z}$*"; }

# Ask a yes/no question; default yes. Returns 0 for yes, 1 for no.
confirm() {
  local prompt="${1:-Continue?}" ans
  read -r -p "$prompt [Y/n] " ans
  case "$ans" in n|N|no|NO) return 1 ;; *) return 0 ;; esac
}

# Pause until the operator presses Enter.
press_enter() { read -r -p "${1:-Press Enter to continue...} "; }

# --- printer / binary -------------------------------------------------------
# Resolve the printer IP from $PRINTER_IP, then $1, else prompt.
require_ip() {
  PRINTER_IP="${PRINTER_IP:-${1:-}}"
  if [ -z "${PRINTER_IP}" ]; then
    read -r -p "Printer IP: " PRINTER_IP
  fi
  [ -n "${PRINTER_IP}" ] || { fail "no printer IP provided"; exit 1; }
  info "Using printer IP: ${PRINTER_IP}"
}

ensure_binary() {
  if [ ! -x "$BINARY" ]; then
    step "Building binary (make build-mac)..."
    ( cd "$REPO_ROOT" && make build-mac ) || { fail "build failed"; exit 1; }
  fi
}

kill_running() {
  pkill -f samsung-scan-macos 2>/dev/null || true
  sleep 1
}

# --- log watching -----------------------------------------------------------
# wait_for_log <logfile> <grep-pattern> <timeout-seconds>
# Returns 0 as soon as the pattern appears, 1 on timeout.
wait_for_log() {
  local logfile="$1" pattern="$2" timeout="${3:-60}" waited=0
  while [ "$waited" -lt "$timeout" ]; do
    if grep -qE -- "$pattern" "$logfile" 2>/dev/null; then return 0; fi
    sleep 1; waited=$((waited + 1))
  done
  return 1
}
