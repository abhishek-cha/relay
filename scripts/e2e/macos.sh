#!/usr/bin/env bash
#
# macOS-only half of the Relay end-to-end suite (spec §21, §23, §54).
#
# Holds the checks that need a real macOS Keychain or a stored browser session:
# the pasted-token auth login/status/logout round-trip, the browser-session
# verbs, and the OAuth2 browser-flow validation plus background refresh.  It also
# re-drives the section 11 MCP call here, because the AUTH_REQUIRED payloads that
# section asserts only exist once a Keychain can refuse to hand back a
# credential.  Runs only on Darwin; scripts/e2e.sh gates it on uname.

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

echo "==> daemon start"
"$workdir/relay" daemon start >/dev/null 2>&1
sleep 1
echo "    install github"
"$workdir/relay" install "$tool" >/dev/null 2>&1

# --- 11. MCP discovery, invocation, and CLI/MCP parity (spec §27, §28, §29) ---
#
# The platform-neutral half drives this same MCP call, but without a Keychain it
# cannot prove the no-credential path: the three checks below read the
# AUTH_REQUIRED payload, and that payload only appears once the daemon can ask
# the Keychain and be told no credential is stored.  This half has its own daemon
# and its own RELAY_HOME, so it installs github and makes the call itself.
echo "    MCP discovery, invocation, and parity (macOS AUTH_REQUIRED re-drive)"

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

check "MCP error payload is AUTH_REQUIRED with a login hint" "ok" "$(python3 - "$mcp_out" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
text = frames.get(3, {}).get("result", {}).get("content", [{}])[0].get("text", "")
err = json.loads(text)
problems = []
if err.get("code") != "AUTH_REQUIRED":
    problems.append("code=" + repr(err.get("code")))
if "relay auth login github" not in err.get("message", ""):
    problems.append("message=" + repr(err.get("message")))
if err.get("retryable") is not False:
    problems.append("retryable=" + repr(err.get("retryable")))
print("ok" if not problems else "bad:" + ";".join(problems))
PY
)"

# Parity is on the code, not the prose.  The CLI here is the installed binary, so
# the comparison crosses the same daemon hop instead of a second route.
github_cli="$RELAY_HOME/tools/github"
github_err="$workdir/mcp.cli.err"
"$github_cli" get-repository --owner openai --repo relay >/dev/null 2> "$github_err"
check "CLI reports github: AUTH_REQUIRED: on stderr" "ok" "$(grep -q '^github: AUTH_REQUIRED:' "$github_err" && echo ok || echo bad)"
check "MCP and CLI report the same error code" "ok" "$(python3 - "$mcp_out" "$github_err" <<'PY'
import json, sys
frames = {}
for line in open(sys.argv[1]):
    if line.strip():
        frame = json.loads(line)
        frames[frame.get("id")] = frame
mcp_code = json.loads(frames[3]["result"]["content"][0]["text"])["code"]
cli_code = ""
for line in open(sys.argv[2]):
    if line.startswith("github: "):
        cli_code = line.split(": ", 2)[1]
        break
print("ok" if mcp_code == cli_code and cli_code else "bad:mcp=%s cli=%s" % (mcp_code, cli_code))
PY
)"

# --- 15. auth login keeps the pasted-token path (spec 21, 54) ---
#
# github declares oauth2 with no device endpoints, so the device authorization
# grant cannot run and `relay auth login` must fall back to the pasted token it
# always used. stdin is a pipe here, so the secret is read as one line and the
# check that nothing echoes it stays meaningful.
echo "    auth login pasted-token fallback"
"$workdir/relay" daemon start >/dev/null 2>&1
sleep 1
login_out="$workdir/auth-login.out"
login_err="$workdir/auth-login.err"
printf 'e2e-token-value\n' | "$workdir/relay" auth login github > "$login_out" 2> "$login_err"
login_exit=$?
check "auth login exits 0 on the pasted-token fallback" "0" "$login_exit"
check "auth login never echoes the secret" "0" "$(cat "$login_out" "$login_err" | grep -c 'e2e-token-value')"
check "auth status reports the credential stored" "true" \
    "$("$workdir/relay" auth status github --json 2>/dev/null | python3 -c 'import json,sys; print(str(json.load(sys.stdin).get("stored", False)).lower())')"
"$workdir/relay" auth logout github >/dev/null 2>&1
check "auth logout clears it again" "false" \
    "$("$workdir/relay" auth status github --json 2>/dev/null | python3 -c 'import json,sys; print(str(json.load(sys.stdin).get("stored", False)).lower())')"

# --- 17 prerequisite: the loopback server and the session-less demo tool ---
#
# Section 9 built 'demo' and section 16 started this loopback server in the
# platform-neutral half.  The browser-session section below needs both, so this
# half stands up its own copies.  Nothing here is checked: the two session-verb
# checks over 'demo' never send a request, and the server only backs the
# browserdemo operations checked inside section 17.
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
  baseUrl: http://127.0.0.1:$sport
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
"$workdir/relay" install "$workdir/demo" >/dev/null 2>&1

# --- 17. browser session verbs and an end-to-end session (spec §23) ---
#
# session_login runs a tool's declared login and stores the cookie; a later
# operation sends it. A tool with no login declared fails loudly, clearing is
# idempotent, and no verb ever prints the cookie.
echo "    browser session (spec §23)"
check "relay session with no subcommand is a usage error" "2" "$("$workdir/relay" session >/dev/null 2>&1; echo $?)"
check "relay session with an unknown subcommand is a usage error" "2" "$("$workdir/relay" session frobnicate demo >/dev/null 2>&1; echo $?)"
check "relay session login with no tool is a usage error" "2" "$("$workdir/relay" session login >/dev/null 2>&1; echo $?)"

no_login_err="$workdir/session-nologin.err"
"$workdir/relay" session login demo >/dev/null 2> "$no_login_err"
check "session login on a tool with no login block fails" "1" "$?"
check "session login on a tool with no login block is INVALID_INPUT" "ok" "$(grep -q INVALID_INPUT "$no_login_err" && echo ok || echo bad)"

check "session clear of a session-less tool is idempotent" "0" "$("$workdir/relay" session clear demo >/dev/null 2>&1; echo $?)"

cat > "$workdir/browserdemo.yaml" <<MANIFEST
apiVersion: relay/v1
kind: Tool
metadata:
  name: browserdemo
  version: 0.0.1
  description: offline browser-session test tool
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: browser
  baseUrl: http://127.0.0.1:$sport
auth:
  type: basic
  login:
    kind: form
    path: /login
    usernameField: user
    passwordField: pass
tools:
  - name: secure
    description: Read a resource that needs the stored session
    input:
      type: object
      properties: {}
    request:
      method: GET
      path: /secure
MANIFEST
"$workdir/relay" build "$workdir/browserdemo.yaml" --out "$workdir/browserdemo" >/dev/null 2>&1
check "browser tool installs" "0" "$("$workdir/relay" install "$workdir/browserdemo" >/dev/null 2>&1; echo $?)"
printf 'alice:secret\n' | "$workdir/relay" auth login browserdemo >/dev/null 2>&1

browser_tool="$RELAY_HOME/tools/browserdemo"
before_secure="$workdir/secure-before.err"
"$browser_tool" secure >/dev/null 2> "$before_secure"
check "browser operation before login is AUTH_REQUIRED" "ok" "$(grep -q AUTH_REQUIRED "$before_secure" && echo ok || echo bad)"
check "browser operation before login points at session login" "ok" "$(grep -q 'relay session login browserdemo' "$before_secure" && echo ok || echo bad)"

browser_login_out="$workdir/session-browser.out"
browser_login_err="$workdir/session-browser.err"
"$workdir/relay" session login browserdemo > "$browser_login_out" 2> "$browser_login_err"
check "relay session login exits 0" "0" "$?"
check "relay session login reports the tool" "ok" "$(grep -q 'logged in browserdemo' "$browser_login_out" && echo ok || echo bad)"
check "relay session login never prints the cookie" "0" "$(cat "$browser_login_out" "$browser_login_err" | grep -c 'session-abc')"

secure_out="$workdir/secure-after.out"
secure_err="$workdir/secure-after.err"
"$browser_tool" secure > "$secure_out" 2> "$secure_err"
check "browser operation after login exits 0" "0" "$?"
check "browser operation sends the stored session" "ok" "$(python3 -c 'import json,sys; print("ok" if json.load(open(sys.argv[1])).get("has_cookie") else "bad")' "$secure_out" 2>&1)"

check "relay session clear browserdemo exits 0" "0" "$("$workdir/relay" session clear browserdemo >/dev/null 2>&1; echo $?)"
revoked_err="$workdir/secure-revoked.err"
"$browser_tool" secure >/dev/null 2> "$revoked_err"
check "browser operation after clear is AUTH_REQUIRED" "ok" "$(grep -q AUTH_REQUIRED "$revoked_err" && echo ok || echo bad)"
"$workdir/relay" auth logout browserdemo >/dev/null 2>&1

# --- 18. OAuth2 browser-flow validation and background refresh (spec §21, §54) ---
echo "    oauth2 browser flow and background refresh (spec §21, §54)"

# github declares oauth2 with neither a browser nor a device flow, so there is
# nothing to exchange.  Refusing to guess is the point: the sweep reports the
# skip on stdout and still exits 0, because a scheduled refresh of a credential
# that cannot be refreshed is not a failure (spec §54).
printf 'e2e-pasted-token\n' | "$workdir/relay" auth login github >/dev/null 2>&1
refresh_all_out="$workdir/auth-refresh-all.out"
refresh_all_err="$workdir/auth-refresh-all.err"
check "refresh --all exits 0 when there is nothing to exchange" "0" "$("$workdir/relay" auth refresh --all > "$refresh_all_out" 2> "$refresh_all_err"; echo $?)"
check "refresh --all reports the skip on stdout" "ok" "$(grep -q 'github: skipped' "$refresh_all_out" && echo ok || echo bad)"
check "refresh --all never prints the token" "0" "$(cat "$refresh_all_out" "$refresh_all_err" | grep -c 'e2e-pasted-token')"
check "refresh with an explicit tool exits 0 too" "0" "$("$workdir/relay" auth refresh github >/dev/null 2>&1; echo $?)"

# Tool and --all are alternatives: neither, or both, is a usage error.
check "refresh with no target is a usage error" "2" "$("$workdir/relay" auth refresh >/dev/null 2>&1; echo $?)"
check "refresh with a tool and --all is a usage error" "2" "$("$workdir/relay" auth refresh github --all >/dev/null 2>&1; echo $?)"

# A pasted token is wrapped in the same envelope an OAuth2 login writes, so it is
# labelled honestly rather than reported as a flow that never ran (spec §21).
status_out="$workdir/auth-status-envelope.out"
"$workdir/relay" auth status github > "$status_out" 2>/dev/null
check "auth status labels a pasted token" "ok" "$(grep -q 'pasted token' "$status_out" && echo ok || echo bad)"
check "auth status never prints the token" "0" "$(grep -c 'e2e-pasted-token' "$status_out")"
"$workdir/relay" auth logout github >/dev/null 2>&1

# Naming an authorization endpoint declares the browser authorization-code flow;
# the manifest must validate with no network access at all.
cat > "$workdir/browserflow.yaml" <<MANIFEST
apiVersion: relay/v1
kind: Tool
metadata:
  name: browserflow
  version: 1.0.0
  description: offline browser-flow validation
protocol:
  type: rest
  baseUrl: https://api.example.test
auth:
  type: oauth2
  authorizationEndpoint: https://example.test/oauth/authorize
  tokenEndpoint: https://example.test/oauth/token
  clientId: relay-e2e
  redirectURI: http://127.0.0.1:8765/callback
tools:
  - name: ping
    description: nothing is sent
    request:
      method: GET
      path: /ping
MANIFEST
check "a browser-flow manifest builds" "0" "$("$workdir/relay" build "$workdir/browserflow.yaml" --out "$workdir/browserflow" >/dev/null 2>&1; echo $?)"

# The loopback redirect is the security boundary: anything a local listener
# cannot receive is refused, and the message names the field but never echoes
# the offending value back into a log.
sed 's#http://127.0.0.1:8765/callback#https://evil.example.test/callback#' \
    "$workdir/browserflow.yaml" > "$workdir/browserflow-bad-redirect.yaml"
bad_redirect_err="$workdir/browserflow-bad-redirect.err"
check "a non-loopback redirect is refused" "1" "$("$workdir/relay" build "$workdir/browserflow-bad-redirect.yaml" --out "$workdir/browserflow-bad" >/dev/null 2>"$bad_redirect_err"; echo $?)"
check "the refusal names the field and not the value" "ok" "$(python3 - "$bad_redirect_err" <<'PY'
import sys
t = open(sys.argv[1]).read()
print("ok" if "auth.redirectURI" in t and "evil.example.test" not in t else "bad")
PY
)"

# The two flows are mutually exclusive; declaring both leaves it ambiguous which
# grant the daemon should own.
sed 's#  tokenEndpoint:.*#  deviceAuthorizationEndpoint: https://example.test/oauth/device\n  tokenEndpoint: https://example.test/oauth/token#' \
    "$workdir/browserflow.yaml" > "$workdir/browserflow-both.yaml"
check "declaring both flows at once is refused" "1" "$("$workdir/relay" build "$workdir/browserflow-both.yaml" --out "$workdir/browserflow-both" >/dev/null 2>&1; echo $?)"

e2e_summary
