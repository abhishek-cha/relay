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

echo "==> M2: daemon lifecycle"

# Build the CLI and daemon once into the scratch dir.
echo "    building relayd and relay"
go build -o "$workdir/relayd" ./cmd/relayd
go build -o "$workdir/relay" ./cmd/relay

# Every daemon test uses its own RELAY_HOME so the user's real ~/.relay is
# never touched.  The trap ensures the daemon is stopped and the temp home is
# torn down even on early exit.
RELAY_HOME="$(mktemp -d)"
export RELAY_HOME
trap '"$workdir/relay" daemon stop >/dev/null 2>&1 || true; rm -rf "$RELAY_HOME"' EXIT

# Prepend the workdir so relay can find its sibling relayd.
PATH="$workdir:$PATH"
export PATH

# --- 1. daemon start succeeds; socket exists ---
echo "    daemon start"
start_exit=$("$workdir/relay" daemon start >/dev/null 2>&1; echo $?)
sleep 1
check "daemon start exits 0" "0" "$start_exit"
check "daemon socket exists" "yes" "$([ -S "$RELAY_HOME/run/daemon.sock" ] && echo yes || echo no)"

# --- 2. daemon status exits 0 and parses as JSON ---
echo "    daemon status"
status_out=$("$workdir/relay" daemon status 2>/dev/null)
status_exit=$?
check "daemon status exits 0" "0" "$status_exit"
check "daemon status is valid JSON" "ok" "$(echo "$status_out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("ok" if "pid" in d and "version" in d else "bad:" + str(list(d.keys())))' 2>&1)"

# --- 3. install registers the tool ---
echo "    install"
install_exit=$("$workdir/relay" install "$tool" >/dev/null 2>&1; echo $?)
check "install exits 0" "0" "$install_exit"
check "registry record exists" "yes" "$([ -f "$RELAY_HOME/registry/github.json" ] && echo yes || echo no)"
check "installed binary exists and is executable" "yes" "$([ -x "$RELAY_HOME/tools/github" ] && echo yes || echo no)"

# --- 4. list mentions github ---
echo "    list"
list_out=$("$workdir/relay" list 2>/dev/null)
check "list exits 0" "0" "$?"
check "list mentions github" "ok" "$(echo "$list_out" | grep -q github && echo ok || echo bad)"

# --- 5. inspect parses as JSON with expected shape ---
echo "    inspect"
inspect_out=$("$workdir/relay" inspect github 2>/dev/null)
check "inspect exits 0" "0" "$?"
check "inspect shape" "ok" "$(echo "$inspect_out" | python3 -c '
import json,sys
d=json.load(sys.stdin)
names = [t["name"] for t in d.get("tools",[])]
ok = d.get("name") == "github" and "get_repository" in names and "list_pull_requests" in names
print("ok" if ok else "bad:name=" + str(d.get("name")) + " tools=" + str(names))
' 2>&1)"

# --- 6. registered tool routes through daemon, gets PROTOCOL_ERROR ---
echo "    registered tool invocation"
check "registered tool gets PROTOCOL_ERROR" "PROTOCOL_ERROR" \
    "$("$RELAY_HOME/tools/github" get-repository --owner openai --repo relay 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"

# --- 7. second daemon start fails (single-instance) ---
echo "    second daemon start"
second_start_exit=$("$workdir/relay" daemon start >/dev/null 2>&1; echo $?)
check "second daemon start fails" "false" "$([ "$second_start_exit" = "0" ] && echo true || echo false)"

# --- 8. unregistered tool reports TOOL_NOT_FOUND ---
echo "    unregistered tool"
cat > "$workdir/ghost.yaml" <<MANIFEST
apiVersion: relay/v1
kind: Tool
metadata:
  name: ghost
  version: 0.0.1
  description: unregistered test tool
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: rest
  baseUrl: https://example.com
tools:
  - name: do_nothing
    description: Does nothing
    input:
      type: object
      properties: {}
    request:
      method: GET
      path: /
MANIFEST
"$workdir/relay" build "$workdir/ghost.yaml" --out "$workdir/ghost" >/dev/null 2>&1
check "unregistered tool gets TOOL_NOT_FOUND" "TOOL_NOT_FOUND" \
    "$("$workdir/ghost" do-nothing 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"

# --- 9. after stop: tool reports NETWORK_ERROR, status fails ---
echo "    daemon stop"
"$workdir/relay" daemon stop >/dev/null 2>&1
sleep 1
check "tool after stop gets NETWORK_ERROR" "NETWORK_ERROR" \
    "$("$RELAY_HOME/tools/github" get-repository --owner openai --repo relay 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "status after stop fails" "false" "$("$workdir/relay" daemon status >/dev/null 2>&1 && echo true || echo false)"

# --- 10. re-install is idempotent ---
echo "    re-install idempotency"
"$workdir/relay" daemon start >/dev/null 2>&1
sleep 1
pre_content=$(cat "$RELAY_HOME/registry/github.json")
reinstall_exit=$("$workdir/relay" install "$tool" >/dev/null 2>&1; echo $?)
post_content=$(cat "$RELAY_HOME/registry/github.json")
check "re-install exits 0" "0" "$reinstall_exit"
check "registry unchanged" "ok" "$([ "$pre_content" = "$post_content" ] && echo ok || echo bad)"

echo
echo "passed: $passed   failed: $failed"
[ "$failed" -eq 0 ]
