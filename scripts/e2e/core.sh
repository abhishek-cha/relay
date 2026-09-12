#!/usr/bin/env bash
#
# Platform-neutral half of the Relay end-to-end suite (spec §9, §10, §52, §60).
#
# Holds every check that does not need a macOS Keychain or a stored browser
# session: the binary contract and descriptor checks that need no daemon, then
# the daemon lifecycle, registry, REST/pagination, telemetry, MCP discovery and
# skill-resource sections.  It runs unchanged on any platform.

set -uo pipefail

source "$(cd "$(dirname "$0")" && pwd)/lib.sh"

e2e_bootstrap

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

# --- 6. registered tool routes through daemon; a known protocol with no
# executor is rejected with PROTOCOL_ERROR ---
#
# The REST executor is wired into the daemon now, so invoking the example tool
# would really leave the machine and hit api.github.com.  This asserts the
# daemon's unimplemented-protocol rejection with a throwaway tool instead
# (spec §19, §20).  The build/install shape mirrors the 'ghost' block in §8.
#
# The fixture declares grpc: a known protocol the validator accepts, but one
# this build registers no executor for.  That combination is the point -- the
# manifest must survive validation and install, then fail at dispatch, so the
# rejection is proven to be the daemon refusing to guess at execution rather
# than the validator turning the manifest away early.  (grpc stands in here
# because graphql used to be executor-less and no longer is.)
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
  type: grpc
  endpoint: https://example.invalid/grpc
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
check "manifest with a known-but-unimplemented protocol validates" "0" \
    "$("$workdir/relay" build "$workdir/unimplemented.yaml" --out "$workdir/unimplemented" >/dev/null 2>&1; echo $?)"
"$workdir/relay" install "$workdir/unimplemented" >/dev/null 2>&1
check "protocol without an executor gets PROTOCOL_ERROR at dispatch" "PROTOCOL_ERROR" \
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
# Capture stats output before matching it. Piping straight into 'grep -q' lets
# grep exit on its first match, which under 'set -o pipefail' kills the
# still-writing stats process with SIGPIPE (exit 141) and reads back as "bad"
# even though stats succeeded. That pipeline race, not a read error, is what
# made these two checks flaky; matching a captured string has no pipe to break.
stats_human="$("$workdir/relay" stats 2>/dev/null)"
check "relay stats renders the human view" "ok" "$([[ "$stats_human" == *get_repo* ]] && echo ok || echo bad)"
stats_tool="$("$workdir/relay" stats --tool demo 2>/dev/null)"
check "relay stats --tool filters to one tool" "ok" "$([[ "$stats_tool" == demo* ]] && echo ok || echo bad)"

# --- 11. MCP discovery, invocation, and CLI/MCP parity (spec §27, §28, §29) ---
#
# Drives `relay mcp` over stdio the way a real client does: one JSON-RPC frame
# per line in, one per line out.  The requests live in a file so the frames stay
# readable, and stdout/stderr are captured separately so a diagnostic on stdout
# or a protocol frame on stderr fails the run instead of vanishing into a merged
# stream.  The daemon is up and github is installed, so discovery and invocation
# exercise the real registry and the real IPC path.
echo "    MCP discovery, invocation, and parity"

mcp_frames="$workdir/mcp.frames.jsonl"
cat > "$mcp_frames" <<'JSONL'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"relay-e2e","version":"0.0.0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"github_get_repository","arguments":{"owner":"openai","repo":"relay"}}}
{"jsonrpc":"2.0","id":4,"method":"ping"}
JSONL

mcp_out="$workdir/mcp.out.jsonl"
mcp_err="$workdir/mcp.err"
"$workdir/relay" mcp < "$mcp_frames" > "$mcp_out" 2> "$mcp_err"
check "relay mcp exits 0 at stdin EOF" "0" "$?"
check "relay mcp keeps stderr empty" "0" "$(wc -c < "$mcp_err" | tr -d ' ')"

# Responses are keyed by id: the notification is fire-and-forget, so exactly the
# four id-bearing frames are answered, once each, with no reply to the
# notification to confuse the client.
check "answers each id once, stays silent on the notification" "ok" "$(python3 - "$mcp_out" <<'PY'
import json, sys
frames, duplicates = {}, []
for line in open(sys.argv[1]):
    if not line.strip():
        continue
    frame = json.loads(line)
    if frame.get("id") in frames:
        duplicates.append(frame.get("id"))
    frames[frame.get("id")] = frame
problems = []
if sorted(frames) != [1, 2, 3, 4]:
    problems.append("ids=" + repr(sorted(frames)))
if duplicates:
    problems.append("duplicate ids=" + repr(duplicates))
problems += ["jsonrpc on id %s" % i for i, f in frames.items() if f.get("jsonrpc") != "2.0"]
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

check "initialize advertises protocolVersion 2025-03-26" "2025-03-26" "$(python3 - "$mcp_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
print(frames.get(1, {}).get("result", {}).get("protocolVersion", ""))
PY
)"

# Discovery is the binary's own projection: every advertised name is
# <tool>_<operation> and every one carries the input schema the CLI validates
# against, so a client can build a call without a second schema source.
check "tools/list advertises <tool>_<operation> names with schemas" "ok" "$(python3 - "$mcp_out" <<'PY'
import json, re, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
tools = frames.get(2, {}).get("result", {}).get("tools", [])
names = [t.get("name", "") for t in tools]
problems = []
if "github_get_repository" not in names or "github_list_pull_requests" not in names:
    problems.append("github ops missing from " + repr(names))
for tool in tools:
    if not re.match(r"^[A-Za-z0-9_-]+_[A-Za-z0-9_]+$", tool.get("name", "")):
        problems.append("unqualified name " + repr(tool.get("name")))
    if not isinstance(tool.get("inputSchema"), dict):
        problems.append("no inputSchema for " + repr(tool.get("name")))
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

# A call with no stored credential must return the structured relay error inside
# the tool result rather than a JSON-RPC protocol fault, so the client branches
# on a code instead of treating a missing credential as a broken server.
check "tools/call with no credential is a tool error, not a protocol fault" "ok" "$(python3 - "$mcp_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
call = frames.get(3, {})
result = call.get("result", {})
content = result.get("content") or [{}]
problems = []
if "error" in call:
    problems.append("protocol fault " + repr(call["error"]))
if result.get("isError") is not True:
    problems.append("isError=" + repr(result.get("isError")))
if content[0].get("type") != "text":
    problems.append("content=" + repr(content)[:80])
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"


# --- 12. after stop: tool reports NETWORK_ERROR, status fails ---
echo "    daemon stop"
"$workdir/relay" daemon stop >/dev/null 2>&1
sleep 1
check "tool after stop gets NETWORK_ERROR" "NETWORK_ERROR" \
    "$("$RELAY_HOME/tools/github" get-repository --owner openai --repo relay 2>&1 >/dev/null | python3 -c 'import sys; print(sys.stdin.read().split(":")[1].strip())')"
check "status after stop fails" "false" "$("$workdir/relay" daemon status >/dev/null 2>&1 && echo true || echo false)"

# --- 13. re-install is idempotent ---
echo "    re-install idempotency"
"$workdir/relay" daemon start >/dev/null 2>&1
sleep 1
pre_content=$(cat "$RELAY_HOME/registry/github.json")
reinstall_exit=$("$workdir/relay" install "$tool" >/dev/null 2>&1; echo $?)
post_content=$(cat "$RELAY_HOME/registry/github.json")
check "re-install exits 0" "0" "$reinstall_exit"
check "registry unchanged" "ok" "$([ "$pre_content" = "$post_content" ] && echo ok || echo bad)"

# --- 14. MCP skill resources: discovery, read, and CLI/MCP parity (spec §8, §29) ---
#
# §29 requires MCP to see the same skill the CLI sees. The daemon is up and
# github is installed, so resources/read travels the real path: MCP asks the
# daemon, the daemon asks the installed binary, and the bytes must equal
# `github --skill`. A crafted URI must be rejected before any registry lookup,
# and an unregistered tool must come back as a structured error, not a crash.
echo "    MCP skill resources"

skill_frames="$workdir/mcp.skill.frames.jsonl"
cat > "$skill_frames" <<'JSONL'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"relay-e2e","version":"0.0.0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"resources/list"}
{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"relay://skill/github"}}
{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"relay://skill/nope"}}
{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"relay://skill/../etc"}}
JSONL

skill_out="$workdir/mcp.skill.out.jsonl"
skill_err_file="$workdir/mcp.skill.err"
"$workdir/relay" mcp < "$skill_frames" > "$skill_out" 2> "$skill_err_file"
skill_exit=$?
check "relay mcp skill run exits 0 at stdin EOF" "0" "$skill_exit"
check "relay mcp skill run keeps stderr empty" "0" "$(wc -c < "$skill_err_file" | tr -d ' ')"

check "resources/list advertises relay://skill/github as text/markdown" "ok" "$(python3 - "$skill_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
resources = frames.get(2, {}).get("result", {}).get("resources")
problems = []
if not isinstance(resources, list):
    problems.append("resources=%r" % (resources,))
else:
    match = [r for r in resources if r.get("uri") == "relay://skill/github"]
    if len(match) != 1:
        problems.append("github entries=%d" % len(match))
    elif match[0].get("mimeType") != "text/markdown":
        problems.append("mimeType=%r" % match[0].get("mimeType"))
    if any("nope" in r.get("uri", "") for r in resources):
        problems.append("unregistered tool advertised")
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

check "resources/read matches github --skill byte for byte" "ok" "$(python3 - "$skill_out" "$tool" <<'PY'
import json, subprocess, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
contents = frames.get(3, {}).get("result", {}).get("contents") or []
cli = subprocess.run([sys.argv[2], "--skill"], capture_output=True, text=True).stdout
problems = []
if len(contents) != 1:
    problems.append("contents=%d" % len(contents))
else:
    entry = contents[0]
    if entry.get("uri") != "relay://skill/github":
        problems.append("uri=%r" % entry.get("uri"))
    if entry.get("mimeType") != "text/markdown":
        problems.append("mimeType=%r" % entry.get("mimeType"))
    if entry.get("text") != cli:
        problems.append("text differs from --skill (%d vs %d bytes)" % (len(entry.get("text") or ""), len(cli)))
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

check "unknown and crafted skill URIs are structured errors" "ok" "$(python3 - "$skill_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
problems = []
unknown = frames.get(4, {}).get("error") or {}
if (unknown.get("data") or {}).get("code") != "TOOL_NOT_FOUND":
    problems.append("unknown tool code=%r" % ((unknown.get("data") or {}).get("code"),))
crafted = frames.get(5, {}).get("error") or {}
if (crafted.get("data") or {}).get("code") != "INVALID_INPUT":
    problems.append("crafted uri code=%r" % ((crafted.get("data") or {}).get("code"),))
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

# --- 16. pagination opt-in (spec §20) ---
#
# A paginated operation's strategy is projected into the executor's spec, but
# the walk is opt-in. Without --paginate the executor makes exactly one request
# and the CLI prints no walk diagnostic; with it the executor follows the
# declared Link chain, aggregates the pages, and reports the walk's shape on
# stderr while stdout stays the machine result alone.
echo "    pagination opt-in (spec §20)"

# A second loopback server carries the paginated and sessionful paths so the
# original REST round-trip above stays byte-for-byte what it was.
sportlog="$workdir/pages.requests"
: > "$sportlog"
cat > "$workdir/pages_server.py" <<'PY'
import json, os, urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

PAGES = {"": ["a", "b"], "2": ["c", "d"], "3": ["e"]}


class Handler(BaseHTTPRequestHandler):
    # Silence the default per-request stderr logging; the request log written
    # below is the only record this test wants.
    def log_message(self, *args):
        pass

    def _log(self, method):
        with open(os.environ["RELAY_E2E_PAGELOG"], "a") as f:
            f.write("%s %s\n" % (method, self.path))

    def _json(self, body, link=None, cookie=None):
        payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        if link:
            self.send_header("Link", link)
        if cookie:
            self.send_header("Set-Cookie", cookie)
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self._log("GET")
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/pages":
            page = urllib.parse.parse_qs(parsed.query).get("page", [""])[0]
            nxt = {"": "2", "2": "3"}.get(page)
            link = '</pages?page=%s>; rel="next"' % nxt if nxt else None
            self._json({"items": PAGES.get(page, [])}, link=link)
            return
        if parsed.path == "/secure":
            cookie = self.headers.get("Cookie", "")
            self._json({"secure": True, "has_cookie": "sid=" in cookie})
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        self._log("POST")
        length = int(self.headers.get("Content-Length", "0") or 0)
        if length:
            self.rfile.read(length)
        if urllib.parse.urlparse(self.path).path == "/login":
            self._json({"ok": True}, cookie="sid=session-abc; Path=/")
            return
        self.send_response(404)
        self.end_headers()


server = HTTPServer(("127.0.0.1", 0), Handler)
with open(os.environ["RELAY_E2E_PAGEPORT"], "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
PY
RELAY_E2E_PAGELOG="$sportlog" RELAY_E2E_PAGEPORT="$workdir/pages.port" \
    python3 "$workdir/pages_server.py" &
session_pid=$!
# Detach from the job table so the trap's kill does not print a job-control
# notice; the PID is tracked explicitly above.
disown "$session_pid" 2>/dev/null || true
for _ in $(seq 1 50); do
    [ -s "$workdir/pages.port" ] && break
    sleep 0.1
done
sport="$(cat "$workdir/pages.port" 2>/dev/null)"
check "pagination loopback server is listening" "yes" "$([ -n "$sport" ] && echo yes || echo no)"

cat > "$workdir/pagedemo.yaml" <<MANIFEST
apiVersion: relay/v1
kind: Tool
metadata:
  name: pagedemo
  version: 0.0.1
  description: offline pagination test tool
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: rest
  baseUrl: http://127.0.0.1:$sport
tools:
  - name: list_pages
    description: List a collection the server pages with Link headers
    input:
      type: object
      properties: {}
    request:
      method: GET
      path: /pages
      pagination:
        style: link-header
MANIFEST
"$workdir/relay" build "$workdir/pagedemo.yaml" --out "$workdir/pagedemo" >/dev/null 2>&1
check "pagedemo tool installs" "0" "$("$workdir/relay" install "$workdir/pagedemo" >/dev/null 2>&1; echo $?)"

# Default off: the declared strategy is still projected, but the request path is
# unchanged -- one request, first page only, no walk diagnostic anywhere.
before=$(wc -l < "$sportlog" | tr -d ' ')
default_out="$workdir/pages-default.out"
default_err="$workdir/pages-default.err"
"$workdir/relay" run pagedemo list_pages > "$default_out" 2> "$default_err"
check "list_pages without --paginate exits 0" "0" "$?"
check "list_pages default makes exactly one request" "1" "$(( $(wc -l < "$sportlog" | tr -d ' ') - before ))"
check "list_pages default hits the first page only" "GET /pages" "$(tail -n 1 "$sportlog")"
check "list_pages default returns the first page alone" "ok" \
    "$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print("ok" if d == {"items": ["a", "b"]} else "bad:" + repr(d))' "$default_out" 2>&1)"
check "list_pages default keeps stderr silent" "0" "$(wc -c < "$default_err" | tr -d ' ')"
check "list_pages default stdout stays machine-only" "ok" "$(grep -q collected "$default_out" && echo bad || echo ok)"

# Opt in: the executor follows the Link chain, aggregates the pages, and the
# walk's shape is reported on stderr while stdout remains the JSON result.
before=$(wc -l < "$sportlog" | tr -d ' ')
paged_out="$workdir/pages.out"
paged_err="$workdir/pages.err"
"$workdir/relay" run pagedemo list_pages --paginate > "$paged_out" 2> "$paged_err"
check "list_pages --paginate exits 0" "0" "$?"
check "list_pages --paginate walks all three pages" "3" "$(( $(wc -l < "$sportlog" | tr -d ' ') - before ))"
check "list_pages --paginate walks the Link chain in order" "GET /pages|GET /pages?page=2|GET /pages?page=3|" "$(tail -n 3 "$sportlog" | tr '\n' '|')"
check "list_pages --paginate aggregates every page" "ok" \
    "$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print("ok" if d == {"items": ["a", "b", "c", "d", "e"]} else "bad:" + repr(d))' "$paged_out" 2>&1)"
check "list_pages --paginate reports the walk on stderr" "ok" "$(grep -q 'collected 3 pages' "$paged_err" && echo ok || echo bad)"
check "list_pages --paginate stdout is still pure JSON" "ok" "$(python3 -c 'import json,sys; json.load(open(sys.argv[1])); print("ok")' "$paged_out" 2>&1)"
check "list_pages --paginate never mixes a diagnostic into stdout" "ok" "$(grep -q 'collected\|relay:' "$paged_out" && echo bad || echo ok)"

# The built tool binary accepts --paginate itself, so a shipped tool walks its
# declared Link chain directly instead of only `relay run` being able to. The
# walk is reported on stderr and stdout stays the pure machine result.
pagedemo_tool="$RELAY_HOME/tools/pagedemo"
before=$(wc -l < "$sportlog" | tr -d ' ')
built_out="$workdir/pages-built.out"
built_err="$workdir/pages-built.err"
"$pagedemo_tool" list_pages --paginate > "$built_out" 2> "$built_err"
check "built tool --paginate exits 0" "0" "$?"
check "built tool --paginate walks all three pages" "3" "$(( $(wc -l < "$sportlog" | tr -d ' ') - before ))"
check "built tool --paginate walks the Link chain in order" "GET /pages|GET /pages?page=2|GET /pages?page=3|" "$(tail -n 3 "$sportlog" | tr '\n' '|')"
check "built tool --paginate aggregates every page" "ok" \
    "$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print("ok" if d == {"items": ["a", "b", "c", "d", "e"]} else "bad:" + repr(d))' "$built_out" 2>&1)"
check "built tool --paginate reports the walk on stderr" "ok" "$(grep -q 'collected 3 pages' "$built_err" && echo ok || echo bad)"
check "built tool --paginate stdout is still pure JSON" "ok" "$(python3 -c 'import json,sys; json.load(open(sys.argv[1])); print("ok")' "$built_out" 2>&1)"
check "built tool --paginate never mixes a diagnostic into stdout" "ok" "$(grep -q 'collected\|pagedemo:' "$built_out" && echo bad || echo ok)"

# --- 16b. MCP pagination parity (spec §20, §27, §29) ---
#
# The CLI gained --paginate; MCP must not be a second execution model that
# silently disagrees. The paginating operation advertises one reserved, optional
# boolean argument, tools/call threads it to the daemon as InvokeRequest.Paginate,
# and the walk's shape (pages, truncated) rides back inside the tool result. The
# same operation through MCP and through `relay run --paginate` must produce the
# same machine result.
echo "    MCP pagination parity (spec §20, §27, §29)"

mcp_pag_frames="$workdir/mcp.paginate.frames.jsonl"
cat > "$mcp_pag_frames" <<'JSONL'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"relay-e2e","version":"0.0.0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pagedemo_list_pages","arguments":{"paginate":true}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"pagedemo_list_pages","arguments":{}}}
JSONL

before=$(wc -l < "$sportlog" | tr -d ' ')
mcp_pag_out="$workdir/mcp.paginate.out.jsonl"
mcp_pag_err="$workdir/mcp.paginate.err"
"$workdir/relay" mcp < "$mcp_pag_frames" > "$mcp_pag_out" 2> "$mcp_pag_err"
check "relay mcp paginated run exits 0 at stdin EOF" "0" "$?"
check "relay mcp paginated run keeps stderr empty" "0" "$(wc -c < "$mcp_pag_err" | tr -d ' ')"

check "MCP advertises paginate only on the paginating operation" "ok" "$(python3 - "$mcp_pag_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
tools = {t.get("name"): t for t in frames.get(2, {}).get("result", {}).get("tools", [])}
paged = (tools.get("pagedemo_list_pages", {}).get("inputSchema") or {})
plain = (tools.get("github_get_repository", {}).get("inputSchema") or {})
paged_props = paged.get("properties") or {}
problems = []
if paged_props.get("paginate", {}).get("type") != "boolean":
    problems.append("paginate=%r" % paged_props.get("paginate"))
if "paginate" in (paged.get("required") or []):
    problems.append("paginate is required")
if "paginate" in (plain.get("properties") or {}):
    problems.append("paginate leaked to a non-paginated operation")
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

check "MCP paginated call matches the CLI result and reports the walk" "ok" "$(python3 - "$mcp_pag_out" "$paged_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
result = frames.get(3, {}).get("result", {})
content = json.loads(result.get("content", [{}])[0].get("text", "null"))
cli = json.load(open(sys.argv[2]))
problems = []
if result.get("isError") is not False:
    problems.append("isError=%r" % result.get("isError"))
if content != cli:
    problems.append("content=%r cli=%r" % (content, cli))
if result.get("pages") != 3:
    problems.append("pages=%r" % result.get("pages"))
if result.get("truncated") is not False:
    problems.append("truncated=%r" % result.get("truncated"))
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

check "MCP call without paginate stays single-page" "ok" "$(python3 - "$mcp_pag_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
result = frames.get(4, {}).get("result", {})
content = json.loads(result.get("content", [{}])[0].get("text", "null"))
problems = []
if content != {"items": ["a", "b"]}:
    problems.append("content=%r" % (content,))
if "pages" in result or "truncated" in result:
    problems.append("walk shape leaked into a single-page result")
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

check "MCP walk plus default call make four requests through the daemon" "4" "$(( $(wc -l < "$sportlog" | tr -d ' ') - before ))"
check "MCP paginated call then the single-page call hit the server in order" \
    "GET /pages|GET /pages?page=2|GET /pages?page=3|GET /pages|" "$(tail -n 4 "$sportlog" | tr '\n' '|')"
check "MCP paginated stdout carries protocol frames only" "ok" "$(python3 - "$mcp_pag_out" <<'PY'
import json, sys
problems = []
for lineno, line in enumerate(open(sys.argv[1]), 1):
    if not line.strip():
        continue
    try:
        frame = json.loads(line)
    except Exception as exc:
        problems.append("line %d: %s" % (lineno, exc))
        continue
    if frame.get("jsonrpc") != "2.0":
        problems.append("line %d is not JSON-RPC 2.0" % lineno)
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

e2e_summary
