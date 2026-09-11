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

# server_pid is set by the offline REST round-trip below.  The trap kills it, so
# the loopback server never outlives the run even on an early exit.
server_pid=""
cleanup() {
    if [ -n "${server_pid:-}" ]; then
        kill "$server_pid" >/dev/null 2>&1 || true
    fi
    "$workdir/relay" daemon stop >/dev/null 2>&1 || true
    rm -rf "$RELAY_HOME"
}
trap cleanup EXIT

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

# --- 6. registered tool routes through daemon; a protocol with no executor is
# rejected with PROTOCOL_ERROR ---
#
# The REST executor is wired into the daemon now, so invoking the example tool
# would really leave the machine and hit api.github.com.  This asserts the
# daemon's unimplemented-protocol rejection with a throwaway tool instead: a
# declared protocol that has no executor yet is rejected rather than guessed at
# (spec §19, §20).  The build/install shape mirrors the 'ghost' block in §8.
echo "    registered tool invocation"
cat > "$workdir/unimplemented.yaml" <<MANIFEST
apiVersion: relay/v1
kind: Tool
metadata:
  name: unimplemented
  version: 0.0.1
  description: throwaway tool declaring a protocol the daemon cannot execute
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: graphql
  endpoint: https://example.invalid/graphql
tools:
  - name: ping
    description: Never reaches an executor
    input:
      type: object
      properties: {}
    request:
      method: POST
      path: /
MANIFEST
"$workdir/relay" build "$workdir/unimplemented.yaml" --out "$workdir/unimplemented" >/dev/null 2>&1
"$workdir/relay" install "$workdir/unimplemented" >/dev/null 2>&1
check "unimplemented protocol gets PROTOCOL_ERROR" "PROTOCOL_ERROR" \
    "$("$RELAY_HOME/tools/unimplemented" ping 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"

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

# --- 9. offline REST round-trip through the daemon ---
# Exercises the full path -- installed tool -> daemon -> REST executor -> a
# loopback server -- with nothing leaving the machine.  The server records each
# request line and serves a fixed JSON body; this is what proves the REST
# executor actually works end to end.
echo "    offline REST round-trip"
reqlog="$workdir/rest.requests"
: > "$reqlog"
cat > "$workdir/rest_server.py" <<'PY'
import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer

BODY = json.dumps({"message": "hello from the relay e2e server", "ok": True}).encode()


class Handler(BaseHTTPRequestHandler):
    # Silence the default per-request stderr logging; the request log written
    # below is the only record this test wants.
    def log_message(self, *args):
        pass

    def do_GET(self):
        with open(os.environ["RELAY_E2E_REQLOG"], "a") as f:
            f.write("GET %s\n" % self.path)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(BODY)))
        self.end_headers()
        self.wfile.write(BODY)


server = HTTPServer(("127.0.0.1", 0), Handler)
with open(os.environ["RELAY_E2E_PORTFILE"], "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
PY
RELAY_E2E_REQLOG="$reqlog" RELAY_E2E_PORTFILE="$workdir/rest.port" \
    python3 "$workdir/rest_server.py" &
server_pid=$!
# Detach from the job table so the trap's kill does not print a job-control
# notice; the PID is tracked explicitly above.
disown "$server_pid" 2>/dev/null || true
# Wait for the server to report the ephemeral port it bound.
for _ in $(seq 1 50); do
    [ -s "$workdir/rest.port" ] && break
    sleep 0.1
done
port="$(cat "$workdir/rest.port" 2>/dev/null)"
check "loopback server is listening" "yes" "$([ -n "$port" ] && echo yes || echo no)"

cat > "$workdir/demo.yaml" <<MANIFEST
apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 0.0.1
  description: offline REST round-trip test tool
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: rest
  baseUrl: http://127.0.0.1:$port
tools:
  - name: get_repo
    description: Get a single repository
    input:
      type: object
      properties:
        owner:
          type: string
        repo:
          type: string
      required:
        - owner
        - repo
    request:
      method: GET
      path: /repos/{owner}/{repo}
  - name: list_repos
    description: List an owner's repositories
    input:
      type: object
      properties:
        owner:
          type: string
        state:
          type: string
      required:
        - owner
    request:
      method: GET
      path: /repos/{owner}/repos
      query:
        state: "{state}"
MANIFEST
"$workdir/relay" build "$workdir/demo.yaml" --out "$workdir/demo" >/dev/null 2>&1
check "demo tool installs" "0" "$("$workdir/relay" install "$workdir/demo" >/dev/null 2>&1; echo $?)"

# Invoke the INSTALLED binary so the whole path runs.
demo="$RELAY_HOME/tools/demo"
get_out="$("$demo" get-repo --owner openai --repo relay 2>"$workdir/demo.err")"
check "get_repo exits 0" "0" "$?"
check "get_repo stdout equals the served JSON" "ok" \
    "$(printf '%s' "$get_out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("ok" if d == {"message": "hello from the relay e2e server", "ok": True} else "bad:" + repr(d))' 2>&1)"
check "get_repo request line" "GET /repos/openai/relay" "$(tail -n 1 "$reqlog")"

"$demo" list-repos --owner openai --state open >/dev/null 2>&1
check "list_repos (with state) exits 0" "0" "$?"
check "list_repos sends the declared query param" "GET /repos/openai/repos?state=open" "$(tail -n 1 "$reqlog")"

"$demo" list-repos --owner openai >/dev/null 2>&1
check "list_repos (without state) exits 0" "0" "$?"
check "list_repos omits an unsupplied query param" "GET /repos/openai/repos" "$(tail -n 1 "$reqlog")"

# --- 10. usage telemetry is local and aggregate (spec §31, §32) ---
echo "    usage telemetry"
telem="$RELAY_HOME/telemetry/usage.jsonl"
check "telemetry stream exists" "yes" "$([ -f "$telem" ] && echo yes || echo no)"
check "every telemetry line is JSON" "ok" "$(python3 - "$telem" <<'PY'
import json, sys
bad = 0
with open(sys.argv[1]) as fh:
    for line in fh:
        if not line.strip():
            continue
        try:
            json.loads(line)
        except Exception:
            bad += 1
print("ok" if bad == 0 else "bad:%d" % bad)
PY
)"
check "telemetry records the demo operations" "ok" "$(python3 - "$telem" <<'PY'
import json, sys
seen = set()
for line in open(sys.argv[1]):
    if line.strip():
        event = json.loads(line)
        if event["tool"] == "demo":
            seen.add(event["operation"])
want = {"get_repo", "list_repos"}
print("ok" if want <= seen else "missing:" + repr(sorted(want - seen)))
PY
)"
# The event surface has no field for input values, so an input like the owner
# name below must never reach disk (spec §32).
check "telemetry carries no request input" "clean" "$(grep -q 'openai' "$telem" && echo leak || echo clean)"
check "relay stats --json counts the invocations" "ok" "$("$workdir/relay" stats --json 2>/dev/null | python3 -c 'import json,sys; t=json.load(sys.stdin)["tools"].get("demo",{}); print("ok" if t.get("total",0) >= 3 and "get_repo" in t.get("operations",{}) else "bad:"+repr(t)[:90])' 2>&1)"
check "relay stats renders the human view" "ok" "$("$workdir/relay" stats 2>/dev/null | grep -q 'get_repo' && echo ok || echo bad)"
check "relay stats --tool filters to one tool" "ok" "$("$workdir/relay" stats --tool demo 2>/dev/null | grep -q '^demo' && echo ok || echo bad)"

# --- 11. after stop: tool reports NETWORK_ERROR, status fails ---
echo "    daemon stop"
"$workdir/relay" daemon stop >/dev/null 2>&1
sleep 1
check "tool after stop gets NETWORK_ERROR" "NETWORK_ERROR" \
    "$("$RELAY_HOME/tools/github" get-repository --owner openai --repo relay 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "status after stop fails" "false" "$("$workdir/relay" daemon status >/dev/null 2>&1 && echo true || echo false)"

# --- 12. re-install is idempotent ---
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
