#!/usr/bin/env bash
# Test 0 — automated pre-flight: vet, unit tests, and both builds.
# No printer required. Run this first.
#
#   ./scripts/manual-tests/00-unit.sh

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

rc=0

info "Test 0 — unit pre-flight (no hardware needed)"

step "go vet ./..."
if ( cd "$REPO_ROOT" && go vet ./... ); then pass "vet clean"; else fail "go vet reported issues"; rc=1; fi

step "go test ./... (incl. the TestRun* recovery suite + snmp contract tests)"
if ( cd "$REPO_ROOT" && go test ./... ); then pass "all tests passed"; else fail "tests failed"; rc=1; fi

step "make build-mac"
if ( cd "$REPO_ROOT" && make build-mac >/dev/null ); then pass "macOS build ok"; else fail "macOS build failed"; rc=1; fi

step "make build-linux"
if ( cd "$REPO_ROOT" && make build-linux >/dev/null ); then pass "linux build ok"; else fail "linux build failed"; rc=1; fi

if [ "$rc" -eq 0 ]; then info "${_G}Test 0 passed.${_Z}"; else info "${_R}Test 0 failed.${_Z}"; fi
exit "$rc"
