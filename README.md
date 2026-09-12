# Relay

**The local capability runtime for AI agents.**

Relay turns an external service or a local capability into one portable executable
that works both as an ordinary CLI and as a tool an AI agent can call. A Relay tool
carries three things:

1. a machine-readable **manifest** - what operations exist and what inputs they require;
2. an embedded **SKILL.md** - how an agent should use those operations; and
3. a generic **runtime** that executes the declared operations.

A long-running **daemon** (`relayd`) supplies the shared infrastructure - credentials,
the macOS Keychain, sessions, HTTP transport, permissions, the registry, telemetry, and
MCP - so no individual tool has to reimplement any of it.

```
Tool = executable + manifest + skill
The daemon is the runtime.
The manifest is machine truth.
The skill is LLM guidance.
MCP is an adapter.
```

The protocol behind a tool - REST today, with GraphQL and gRPC on the same seam - is an
implementation detail. The manifest and the CLI contract are the product; the language a
tool is implemented in is not.

## Two ways in

| You want to | Start at |
| --- | --- |
| **Use** a tool someone else built - give your shell and your MCP client access to an API | [Quickstart](#quickstart) |
| **Package** your own API as a CLI + MCP tool | [Package your own API](#package-your-own-api) |

## How it fits together

```
                  AI agent / MCP client
                           | stdio (JSON-RPC)
                   +-------v-------+
                   |   relay mcp   |  adapter - no execution logic of its own
                   +-------+-------+
                           | Unix-socket IPC
                   +-------v-------+
                   |     relayd    |  auth, Keychain, sessions, permissions,
                   +-------+-------+  registry, executors, telemetry
                           |
        +------------------+------------------+
        v                  v                  v
     github             slack            yourtool
   (built tool)       (built tool)      (built tool)
        +---- embedded manifest + SKILL.md, one generic runtime ----+
```

A tool binary never holds a credential and never calls a service directly. `yourtool
list-things` on the command line and `yourtool_list_things` over MCP travel the same path
to the same daemon, so they return the same result and the same structured errors.

---

## Quickstart

Prerequisites: macOS, plus the `relay` and `relayd` binaries on your PATH. There is no
`brew` formula yet, so build them from source with `make build` (which writes `dist/relay`
and `dist/relayd`) and put that directory on your PATH.

```sh
# 1. Start the daemon. It runs detached and logs to ~/.relay/logs/.
relay daemon start
relay daemon status

#    To bring it back after a reboot, write the LaunchAgent and load it:
relay daemon install
launchctl load ~/Library/LaunchAgents/com.relay.daemon.plist

# 2. Install a prebuilt tool. This copies the binary, re-reads its descriptor,
#    checks runtime compatibility, and registers it.
relay install ./github
relay list

# 3. Log in if the tool declares auth.
relay auth login github
relay auth status github

# 4. Use it as a normal CLI: stdout is the JSON result, stderr is diagnostics.
github get-repository --owner openai --repo relay | jq .name

# 5. Point an MCP client at it (config below).
```

The MCP transport is stdio, so the client spawns Relay as a subprocess. Use an absolute
path - macOS GUI apps do not inherit your shell `PATH`, which is the usual reason this
silently fails to start:

```json
{
  "mcpServers": {
    "relay": {
      "command": "/usr/local/bin/relay",
      "args": ["mcp"]
    }
  }
}
```

That exposes every registered tool as `<tool>_<operation>` and each tool's `SKILL.md` as a
resource. `relay mcp --log /tmp/relay-mcp.log` sends diagnostics to a file; stdout stays
protocol-only either way.

Two things that surprise people:

- **`relay install` is the only way a tool becomes reachable.** Registration is explicit
  by design; Relay never auto-enrols a binary it happens to find.
- **The daemon must be running to invoke anything.** Discovery (`tools/list`) reads the
  registry and each binary's `--describe`, so it still works with the daemon down, but a
  call then fails with `NETWORK_ERROR`.

---

## Package your own API

The whole product is reachable from two files. Start from the templates:

```sh
mkdir mytool && cd mytool
cp path/to/relay/templates/tool.yaml mytool.yaml
cp path/to/relay/templates/SKILL.md SKILL.md
```

### 1. Write the manifest

`mytool.yaml` is machine truth: which operations exist, what inputs they take, and how to
reach them. Nothing else belongs here.

```yaml
apiVersion: relay/v1
kind: Tool
metadata:
  name: mytool
  version: 0.1.0
  description: Read and search things in Example's API

runtime:
  name: relay
  apiVersion: v1

protocol:
  type: rest
  baseUrl: https://api.example.com

auth:
  type: bearer

capabilities:
  - network
  - keychain

permissions:
  network:
    hosts:
      - api.example.com

tools:
  - name: get_thing
    description: Get one thing by id
    input:
      type: object
      properties:
        id:
          type: string
          description: The thing's id
      required:
        - id
    request:
      method: GET
      path: /things/{id}

  - name: search_things
    description: Search things by name
    input:
      type: object
      properties:
        q:
          type: string
          description: Free-text query
        limit:
          type: integer
          description: Page size (1-100)
          default: 20
        cursor:
          type: string
          description: Opaque cursor from a previous page
    request:
      method: GET
      path: /things
      query:
        q: "{q}"
        limit: "{limit}"
      pagination:
        style: cursor
        cursorParam: cursor
        cursorField: next_cursor
        cursorIn: query
```

Three rules worth internalizing now:

- A `{param}` in `request.path` must name a declared property **and** appear in
  `input.required`. The build refuses otherwise, because a path you cannot fill is a
  broken operation.
- Inputs named in `request.query` or `request.headers` are substituted from the same
  input object.
- On a method that carries a body, **every remaining input is sent as a JSON body**. That
  is why most POST operations need no explicit `body` block.

### 2. Write the skill

`SKILL.md` answers the other question: *how* should an agent use these operations. It is
guidance, never a second schema.

```markdown
# mytool

Use this tool to look up and search things in Example's API.

## Recommended workflows

1. `get_thing` when you already have an id - it is the cheapest call.
2. `search_things` when you have a name but no id, then `get_thing` on the hit.

## Choosing between operations

`search_things` answers "which thing?" and `get_thing` answers "what about this thing?".
If you already have an id, do not search for it.

## Pagination

`search_things` returns at most `limit` results plus a cursor for the next page. Pass it
back as `cursor` until the response stops returning one. Prefer a small `limit` and more
pages over one huge page.

## Interpreting results

An empty `results` array is a valid answer, not an error. Example's API reports
application-level failures in an `error` field on an HTTP 200 response, so check that
field rather than trusting the status code alone.
```

A build-time linter rejects a skill that restates the manifest's inputs, because two
sources of truth drift. It also checks the names you write in backticks against the
manifest, so a snake_case token that is neither a declared operation nor a declared input
property is reported as a broken pointer - which is why the example above describes the
response's cursor in prose instead of backticking a field name. Optionally pin the skill
to the manifest with YAML frontmatter (`version: 0.1.0`); when present it must equal
`metadata.version`.

### 3. Build

```sh
relay build mytool.yaml --skill SKILL.md
# -> dist/mytool, plus a PATH shim at ~/.relay/bin/mytool
```

The result is self-contained: no YAML, no Go, and no source tree are needed at the
destination.

### 4. Inspect it without the daemon

```sh
./dist/mytool --describe     # the machine contract, as JSON
./dist/mytool --skill        # the embedded SKILL.md
./dist/mytool --help
./dist/mytool --version
```

### 5. Run it

```sh
relay daemon start
relay install ./dist/mytool
relay auth login mytool          # the secret goes to the Keychain, via the daemon

mytool get-thing --id 42
mytool --json search-things --q widget --limit 5
```

### 6. Ship it

Hand over `dist/mytool`. The recipient runs `relay install ./mytool` and the tool is
immediately available as both a CLI and an MCP tool. They need the Relay runtime; they do
not need your YAML, your source, or a compiler.

For a tamper check, sign it:

```sh
relay keygen --out keys/
relay sign dist/mytool --key keys/relay.key --publisher "Example Inc"
relay install dist/mytool        # verifies the sidecar .sig before copying anything
```

---

## Manifest at a glance

A manifest has three parts: who the tool is, how to reach the service, and what
operations exist. The walkthrough above covers the common shape; the complete field
reference — every protocol, credential type, and pagination style — lives in
[`docs/CAPABILITIES.md`](docs/CAPABILITIES.md).

| Key | Required | Notes |
| --- | --- | --- |
| `apiVersion` | yes | exactly `relay/v1` |
| `kind` | yes | exactly `Tool` |
| `metadata.name` | yes | lowercase snake_case; becomes the binary name |
| `metadata.version` | yes | paired with the skill's version |
| `metadata.description` | no | one line |
| `runtime` | no | `{name: relay, apiVersion: v1}`; a mismatch is refused at install |
| `protocol` | yes | `rest`, `graphql`, `grpc`, or `browser`, plus that protocol's address |
| `auth` | no | the credential the daemon must hold; omit it for a tool that needs none |
| `capabilities` | no | opt in to the capability model |
| `permissions` | no | narrows what those capabilities may touch |
| `tools` | yes | at least one operation |

Each operation needs a snake_case `name`, a `description`, an `input` schema, and a
`request` shaped by the protocol. Everything else — the other protocols, the OAuth2 and
browser login setups, pagination, and the on-disk config files — is in the
[capabilities reference](docs/CAPABILITIES.md).

## Authentication

Credentials are owned by the daemon and stored in the macOS Keychain under
`com.relay.<tool>`. The manifest declares only *what kind* of credential is required; the
secret never appears in the manifest, and never reaches the tool binary, so it cannot leak
into stdout, a log, telemetry, or MCP output.

```sh
relay auth login mytool          # store a credential
relay auth status mytool         # the type, expiry, scope, and whether it refreshes
relay auth refresh --all         # rotate what is due (safe to schedule; a skip exits 0)
relay auth logout mytool         # forget it
```

The five credential types — `api_key`, `bearer`, `basic`, `client_credentials`, and
`oauth2` — all live in the manifest's `auth` block and all reach the wire the same way,
attached by the daemon rather than by the tool. For OAuth2 there are three login flavours
(paste a token, device grant, or browser with PKCE) and one command; the manifest picks
the flow. The [capabilities reference](docs/CAPABILITIES.md#credential-setups) has each
one in full.

---

## The binary contract

Every built tool implements the same stable interface:

| Flag | Output | Purpose |
| --- | --- | --- |
| `--describe` | JSON on stdout | the registration and discovery descriptor |
| `--manifest` | raw embedded YAML | the authoritative schema source |
| `--skill` | embedded `SKILL.md` | the LLM guidance the binary carries |
| `--help` | human help | usage and the operation list |
| `--version` | `<name> <version>` | tool identity |
| `--json` | JSON envelope | wraps a result or an error for machine consumers |
| `--paginate` | stderr note | walks an operation's declared pages; accepted only where the manifest declares a pagination strategy |

## The output contract

| Stream | Carries |
| --- | --- |
| `stdout` | the machine result - JSON, and nothing else |
| `stderr` | diagnostics, logs, and human-readable errors |
| exit code | `0` success, `1` operation error, `2` usage error |

Logs never mix into JSON on stdout, so a result pipes straight into `jq`. A failed
operation reports a structured error with a stable code - `INVALID_INPUT`,
`AUTH_REQUIRED`, `PERMISSION_DENIED`, `RATE_LIMITED`, `TIMEOUT`, `NETWORK_ERROR`,
`REMOTE_ERROR`, `PROTOCOL_ERROR`, and others - identically on the CLI and over MCP. That
predictability is the point: an agent can branch on the code rather than parse prose.

## Command surface

`relay` is the only CLI you need; `relay --help` and each command's `-h` are authoritative.

| Command | Usage |
| --- | --- |
| `relay build` | `relay build <manifest.yaml> [--skill SKILL.md] [--out PATH] [--source DIR] [--keep]` |
| `relay install` | `relay install <path>` |
| `relay list` | `relay list` |
| `relay inspect` | `relay inspect <tool>` |
| `relay run` | `relay run <tool> <operation> [--input JSON] [--paginate]` |
| `relay daemon` | `relay daemon <start\|stop\|restart\|status\|install>` |
| `relay logs` | `relay logs [-n LINES]` (default 50) |
| `relay mcp` | `relay mcp [--log PATH]` |
| `relay auth` | `relay auth <login\|logout\|status\|refresh> <tool>`; `login` takes `--type`, `status` takes `--json`, `refresh` takes `--all`, `--force`, `--json` |
| `relay session` | `relay session <login\|clear> <tool>` for browser-backed tools |
| `relay stats` | `relay stats [--json] [--tool NAME] [--sequences N]` |
| `relay keygen` | `relay keygen [--out DIR] [--force]` |
| `relay sign` | `relay sign <binary> --key KEYFILE [--publisher NAME] [--out PATH]` |
| `relay version` | `relay version` |

There is no `uninstall` command; `relay install` is the only registry mutation the CLI
exposes.

---

## Repository layout

```
relay/
├── cmd/
│   ├── relay/          # the CLI: build, install, daemon, mcp, auth, session
│   ├── relayd/         # the daemon
│   └── relay-tool/     # the generic runtime embedded into every built tool
├── internal/
│   ├── manifest/       # parse + validate the manifest (machine truth)
│   ├── runtime/        # tool CLI: flags, input validation, output contract
│   ├── build/          # manifest + skill -> one self-contained binary
│   ├── skill/          # skill validation and the no-schema linter
│   ├── daemon/         # socket server, invocation, registry, telemetry
│   ├── registry/       # on-disk discovery records (never a schema source)
│   ├── ipc/            # Unix-socket framing
│   ├── auth/           # credential resolution, device + browser OAuth2
│   ├── keychain/       # macOS Keychain integration
│   ├── browser/        # sessions, login, cookies
│   ├── protocol/       # the Executor seam: rest, graphql, grpc, browser
│   ├── mcp/            # MCP adapter over the same execution path
│   ├── telemetry/      # local, privacy-first usage events
│   ├── permissions/    # capability and permission policy
│   ├── paths/          # the ~/.relay layout
│   └── fsutil/         # file copy and permission helpers
├── pkg/
│   ├── relay/          # public wire types + the error model
│   └── toolruntime/    # the runtime embedded into every built tool
├── templates/          # manifest + SKILL.md starting points
├── examples/           # worked examples: github, slack, stripe, filesystem
├── docs/               # DESIGN.md, CONTRIBUTING.md
├── scripts/e2e.sh      # end-to-end suite
├── TASKS.md            # the phased build-out checklist
└── README.md
```

`dist/` holds built binaries; `~/.relay/` holds installed tools, the daemon socket, the
registry, config, and logs. Both are generated.

## Documentation

- [`docs/CAPABILITIES.md`](docs/CAPABILITIES.md) - every manifest field: protocols, credential setups, capabilities, pagination, and the config files.
- [`docs/DESIGN.md`](docs/DESIGN.md) - the complete design spec.
- [`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md) - how to add and verify a tool.
- [`examples/`](examples/) - worked manifests, including the caveats each one carries.
- [`TASKS.md`](TASKS.md) - the phased checklist with acceptance criteria.

## Status

The core loop works end to end: write a manifest, build a tool, inspect it, run the
daemon, install the tool, and execute the same capability through the CLI, through the
tool binary, and through MCP. REST, GraphQL, gRPC, and browser tools each have an executor
wired into the daemon behind one seam.

Credentials live in the macOS Keychain and are managed with `relay auth`; `relay stats`
reads the local telemetry stream. The daemon enforces declared capabilities and
permissions before it reads a credential or dispatches an executor. That model is opt-in
per tool: a manifest that declares neither `capabilities` nor `permissions` runs
unchecked, and one that declares either is then held default-deny.

Not built yet, and deliberately so: a hosted registry and `relay install <name>`, a
`brew` formula, telemetry that leaves the machine, and automatic skill generation.
