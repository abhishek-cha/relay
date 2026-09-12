#!/usr/bin/env bash
#
# Shared helpers for the Relay end-to-end suite (spec §9, §10, §52, §60).
#
# Sourced by scripts/e2e/core.sh and scripts/e2e/macos.sh.  Each half sources
# this file, so each half gets its own workdir, RELAY_HOME, daemon, and cleanup
# trap -- a failure in one half cannot cascade into the other.

# --- result counters ---------------------------------------------------------
#
# Defined at source time so every check and the final summary in a half share
# one pair of counters, regardless of where in the half they run.
passed=0
failed=0

check() {
    local label="$1"
    local expected="$2"
    local actual="$3"
    if [ "$expected" = "$actual" ]; then
        printf '  ok    %s\n' "$label"
        passed=$((passed + 1))
    else
        printf '  FAIL  %s (expected %s, got %s)\n' "$label" "$expected" "$actual"
        failed=$((failed + 1))
    fi
}

# --- summary -----------------------------------------------------------------
#
# Run standalone: prints the historical one-line summary and returns the
# verdict as the exit status.
#
# Run by scripts/e2e.sh (E2E_AGGREGATE set): records the counts in
# E2E_COUNTS_FILE so the entrypoint can add the halves together, and stays quiet
# so exactly one summary line reaches the user.
e2e_summary() {
    if [ -n "${E2E_COUNTS_FILE:-}" ]; then
        printf '%s\n%s\n' "$passed" "$failed" > "$E2E_COUNTS_FILE"
    fi
    if [ -z "${E2E_AGGREGATE:-}" ]; then
        echo "passed: $passed   failed: $failed"
    fi
    [ "$failed" -eq 0 ]
}

# --- bootstrap ---------------------------------------------------------------
#
# Shared preamble: resolve the repo root, mktemp a scratch workdir, build relayd
# and relay into it, stand up a private RELAY_HOME, put the build dir on PATH,
# and install the cleanup trap that kills the loopback servers and stops the
# daemon.  Called once by each half.
e2e_bootstrap() {
    root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
    cd "$root"

    workdir="$(mktemp -d)"
    # The scratch directory is left in place so a failing run can be inspected.

    echo "    building relayd and relay"
    go build -o "$workdir/relayd" ./cmd/relayd
    go build -o "$workdir/relay" ./cmd/relay

    # Every daemon test uses its own RELAY_HOME so the user's real ~/.relay is
    # never touched.  The trap ensures the daemon is stopped and the temp home is
    # torn down even on early exit.
    RELAY_HOME="$(mktemp -d)"
    export RELAY_HOME

    # server_pid is set by an offline REST round-trip.  The trap kills it, so
    # the loopback server never outlives the run even on an early exit.
    server_pid=""
    session_pid=""
    cleanup() {
        if [ -n "${server_pid:-}" ]; then
            kill "$server_pid" >/dev/null 2>&1 || true
        fi
        if [ -n "${session_pid:-}" ]; then
            kill "$session_pid" >/dev/null 2>&1 || true
        fi
        "$workdir/relay" daemon stop >/dev/null 2>&1 || true
        rm -rf "$RELAY_HOME"
    }
    trap cleanup EXIT

    # Prepend the workdir so relay can find its sibling relayd.
    PATH="$workdir:$PATH"
    export PATH
}
