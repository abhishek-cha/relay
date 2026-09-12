#!/usr/bin/env bash
#
# One-command entrypoint for the Relay end-to-end suite (spec §9, §10, §52, §60).
#
# Runs the platform-neutral half always, and the macOS-only half -- the checks
# that need a real Keychain or a stored browser session -- only on Darwin.  Each
# half runs in its own process with its own workdir, RELAY_HOME, daemon, and
# cleanup trap, so a failure in one cannot take the other down.  This script
# aggregates both halves' counts into a single summary line and exits non-zero
# if anything failed.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
core="$here/e2e/core.sh"
macos_half="$here/e2e/macos.sh"

total_passed=0
total_failed=0
any_failed=0

# Run one half as a child process.  E2E_AGGREGATE tells its e2e_summary to stay
# quiet and record counts in E2E_COUNTS_FILE; the half's own check output is
# replayed here so the run still reads like the original single script.
run_half() {
    local script="$1"
    local counts out rc c_passed c_failed
    counts="$(mktemp)"
    out="$(E2E_AGGREGATE=1 E2E_COUNTS_FILE="$counts" bash "$script" 2>&1)"
    rc=$?
    printf '%s\n' "$out"
    c_passed=0
    c_failed=0
    if [ -s "$counts" ]; then
        c_passed="$(sed -n '1p' "$counts")"
        c_failed="$(sed -n '2p' "$counts")"
    fi
    total_passed=$((total_passed + c_passed))
    total_failed=$((total_failed + c_failed))
    if [ "$rc" -ne 0 ] || [ "$c_failed" -ne 0 ]; then
        any_failed=1
    fi
}

echo "==> platform-neutral core (scripts/e2e/core.sh)"
run_half "$core"

if [ "$(uname -s)" = "Darwin" ]; then
    echo "==> macOS-only checks (scripts/e2e/macos.sh)"
    run_half "$macos_half"
else
    echo "==> skipping macOS-only checks (Keychain/session): $(uname -s) is not Darwin"
fi

echo "passed: $total_passed   failed: $total_failed"
[ "$any_failed" -eq 0 ]
