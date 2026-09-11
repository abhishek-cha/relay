#!/usr/bin/env bash
#
# End-to-end checks for the Relay binary contract (spec §9, §10, §52, §60).
#
# Builds the example GitHub tool and asserts the parts of the contract that do
# not need a running daemon: --describe, --skill, --version, --help, generated
# operations, input validation, the structured error model, and portability.
#
# Milestones extend this script as they land.

set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

workdir="$(mktemp -d)"
# The scratch directory is left in place so a failing run can be inspected.

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

echo "==> building the example tool"
tool="$workdir/github"
if ! go run ./cmd/relay build examples/github/github.yaml \
        --skill examples/github/SKILL.md --out "$tool" 2>"$workdir/build.log"; then
    echo "build failed:"
    cat "$workdir/build.log"
    exit 1
fi
echo "    built $tool"

echo "==> binary contract"
check "--describe exits 0" "0" "$("$tool" --describe >/dev/null 2>&1; echo $?)"
check "--skill exits 0" "0" "$("$tool" --skill >/dev/null 2>&1; echo $?)"
check "--version exits 0" "0" "$("$tool" --version >/dev/null 2>&1; echo $?)"
check "--help exits 0" "0" "$("$tool" --help >/dev/null 2>&1; echo $?)"
check "no arguments is a usage error" "2" "$("$tool" >/dev/null 2>&1; echo $?)"

echo "==> descriptor shape (spec §9)"
check "descriptor fields" "ok" "$(python3 - "$tool" <<'PY'
import json, subprocess, sys
d = json.loads(subprocess.run([sys.argv[1], "--describe"], capture_output=True, text=True).stdout)
want = {"apiVersion": "relay/v1", "kind": "Tool", "name": "github",
        "version": "1.0.0", "protocol": "rest", "skill": True}
problems = [k for k, v in want.items() if d.get(k) != v]
problems += [] if d.get("runtime", {}).get("apiVersion") == "v1" else ["runtime.apiVersion"]
names = [t["name"] for t in d.get("tools", [])]
problems += [] if names == ["get_repository", "list_pull_requests"] else ["tools"]
print("ok" if not problems else "bad:" + ",".join(map(str, problems)))
PY
)"

echo "==> skill is embedded (spec §8)"
check "skill is non-empty guidance" "ok" "$("$tool" --skill | python3 -c 'import sys; t=sys.stdin.read(); print("ok" if t.strip().startswith("#") else "bad")')"
check "skill matches source" "ok" "$(cmp -s <("$tool" --skill) examples/github/SKILL.md && echo ok || echo bad)"

echo "==> generated operations (spec §10, §11)"
# The daemon is not running yet, so a well-formed call must reach it and fail
# with a retryable transport error rather than a usage or parse error.
check "valid call fails only on transport" "NETWORK_ERROR" \
    "$("$tool" get-repository --owner openai --repo relay 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "snake_case alias resolves" "NETWORK_ERROR" \
    "$("$tool" get_repository --owner openai --repo relay 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "--json envelope on failure" "false" \
    "$("$tool" --json get-repository --owner openai --repo relay 2>/dev/null | python3 -c 'import json,sys; print(str(json.load(sys.stdin)["success"]).lower())')"
check "missing required input" "INVALID_INPUT" \
    "$("$tool" get-repository --owner openai 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "unknown operation" "OPERATION_NOT_FOUND" \
    "$("$tool" frobnicate 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "unknown flag" "INVALID_INPUT" \
    "$("$tool" get-repository --owner a --repo b --bogus 1 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "input-json path" "NETWORK_ERROR" \
    "$("$tool" get-repository --input-json '{"owner":"a","repo":"b"}' 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "input file path" "NETWORK_ERROR" \
    "$(printf '{"owner":"a","repo":"b"}' > "$workdir/in.json"; "$tool" get-repository --input "$workdir/in.json" 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "--input and --input-json conflict" "INVALID_INPUT" \
    "$("$tool" get-repository --input "$workdir/in.json" --input-json '{}' 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"

echo "==> portability (spec §34)"
moved="$workdir/elsewhere/github"
mkdir -p "$workdir/elsewhere"
cp "$tool" "$moved"
check "runs with no source tree present" "0" "$(cd "$workdir/elsewhere" && ./github --describe >/dev/null 2>&1; echo $?)"
check "carries its own skill" "ok" "$(cd "$workdir/elsewhere" && ./github --skill | head -1 | grep -q '^# GitHub' && echo ok || echo bad)"

echo
echo "passed: $passed   failed: $failed"
[ "$failed" -eq 0 ]
