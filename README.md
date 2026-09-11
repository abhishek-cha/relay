# Relay

**The local capability runtime for AI agents.**

Relay turns external services and local capabilities into self-describing,
AI-native tools. A Relay tool is a single portable executable that carries
three things:

1. a machine-readable **manifest** — what operations exist and what inputs they require;
2. an embedded **SKILL.md** — how an AI agent should use those operations; and
3. a generic **runtime** that executes the declared operations.

A long-running **daemon** provides the shared infrastructure: auth, the macOS
Keychain, sessions, HTTP transport, permissions, the registry, telemetry, and MCP.

Everything Relay does rests on five invariants:

```
Tool = executable + manifest + skill
The daemon is the runtime.
The manifest is machine truth.
The skill is LLM guidance.
MCP is an adapter.
```

The underlying protocol — REST and GraphQL today, more later — is an
implementation detail behind one executor seam. The manifest and the CLI
contract are the product; the implementation language is not.

## Architecture

```
                    AI agent / MCP client
                             │  stdio (JSON-RPC)
                     ┌───────▼───────┐
                     │   relay mcp   │  MCP adapter — no execution logic of its own
                     └───────┬───────┘
                             │  Unix-socket IPC
                     ┌───────▼───────┐
                     │     relayd    │  the runtime: auth, Keychain, permissions,
                     └───────┬───────┘  registry, REST executor, telemetry
                             │
          ┌──────────────────┼──────────────────┐
          ▼                  ▼                  ▼
       github             slack            filesystem
     (built tool)       (built tool)      (local capability)
          └──── embedded manifest + SKILL.md, one generic runtime ────┘
```

Tool binaries never call a service directly: `github get-repository` and an MCP
`github_get_repository` both travel the same path to the daemon.

## Quickstart

Prerequisites: the Go toolchain (1.22+; this tree is verified with Go 1.27.1 at
`/usr/local/go/bin`) and macOS for the Keychain-backed pieces.

```sh
# 1. Build relay and relayd, then put them on PATH for this shell.
export PATH="$PATH:/usr/local/go/bin"
make build                       # -> dist/relay, dist/relayd
export PATH="$PWD/dist:$PATH"
relay version

# 2. Build a tool from a manifest and a skill.
relay build examples/github/github.yaml --skill examples/github/SKILL.md
#    The build prints the binary path it wrote. By default that is dist/github,
#    and the same build links the tool into ~/.relay/bin.

# 3. Inspect the built binary. This needs no daemon.
./dist/github --describe         # the machine contract, as JSON
./dist/github --skill            # the embedded SKILL.md
./dist/github --help
./dist/github --version

# 4. Start the daemon and check it.
relay daemon start
relay daemon status

# 5. Install the tool: register it and expose it on PATH.
relay install ./dist/github
relay list

# 6. Run an operation. It routes through the daemon.
github get-repository --owner openai --repo relay

# 7. Expose everything to an MCP client.
relay mcp                        # stdio MCP server over the same path
```

Two notes on that sequence:

- **Where the binary lands.** A default build writes `dist/<tool>` and drops a
  PATH shim in `~/.relay/bin`, so the bare name works once that directory is on
  your PATH. An explicit `--out ./github` skips the shim and writes exactly
  where you asked.
- **Why step 6 may fail at first.** `github` declares auth, so the daemon needs
  a stored credential before it will call the API (`relay auth login github`,
  spec §21). Without one the invocation returns `AUTH_REQUIRED` — the schema
  and routing are still doing their job; only the secret is missing.

## Command surface

`relay` is the only CLI; every flag below is the complete set the binary accepts
(`relay --help` and each command's `-h` are authoritative).

| Command | Usage |
| --- | --- |
| `relay build` | `relay build <manifest.yaml> [--skill SKILL.md] [--out PATH] [--source DIR] [--keep]` |
| `relay install` | `relay install <path>` |
| `relay list` | `relay list` |
| `relay inspect` | `relay inspect <tool>` |
| `relay daemon` | `relay daemon <start\|stop\|restart\|status\|install>` |
| `relay logs` | `relay logs [-n LINES]` (default 50) |
| `relay mcp` | `relay mcp [--log PATH]` |
| `relay auth` | `relay auth <login\|logout\|status> <tool>` (`login` takes `--type`, `status` takes `--json`) |
| `relay stats` | `relay stats [--json] [--tool NAME] [--sequences N]` |
| `relay version` | `relay version` |

There is no `uninstall` command: `relay install` is the only registry mutation
the CLI exposes.

## Connect an MCP client

`relay mcp` speaks MCP over stdio and exposes every registered tool as
`<tool>_<operation>` — for example `github_get_repository`. A client config
looks like this:

```json
{
  "mcpServers": {
    "relay": {
      "command": "relay",
      "args": ["mcp"]
    }
  }
}
```

There is no MCP-only execution path: an MCP call and the equivalent CLI call run
the same operation through the same daemon, so they return the same result and
the same structured errors (spec §27–§29, §41).

## Binary contract

Every built tool binary implements the same stable interface (spec §9, §10):

| Flag | Output | Purpose |
| --- | --- | --- |
| `--describe` | JSON on stdout | Registration/discovery descriptor (name, version, protocol, operations) |
| `--manifest` | raw embedded YAML | The authoritative schema source for the operation inputs |
| `--skill` | embedded `SKILL.md` | The LLM guidance the binary carries |
| `--help` | human help on stdout | Usage and the operation list |
| `--version` | `<name> <version>` | Tool identity |
| `--json` | JSON envelope | Wraps a result or an error for machine consumers |

Operations are typed with the tool's own name: the manifest's `get_repository`
becomes `get-repository` on the command line, and each input property becomes a
flag (spec §11). A complex input also accepts `--input <file.json>` or
`--input-json '<json>'`; an explicitly passed flag wins over either.

## Output contract

Every invocation obeys one contract (spec §10):

| Stream | Carries |
| --- | --- |
| `stdout` | the machine result — JSON, and nothing else |
| `stderr` | diagnostics, logs, and human-readable errors |
| exit code | `0` success · `1` operation error · `2` usage error |

Logs never mix into JSON on stdout, so a result can be piped straight into
`jq`. Failed operations report a structured error with a stable code (for
example `INVALID_INPUT`, `AUTH_REQUIRED`, `RATE_LIMITED`, `PROTOCOL_ERROR`,
`REMOTE_ERROR`), identically to the CLI and to MCP (spec §26).

## Repository layout

```
relay/
├── cmd/
│   ├── relay/          # developer/user CLI: build, install, daemon, mcp, auth
│   ├── relayd/         # the daemon
│   └── relay-tool/     # the generic tool runtime (the embedded runtime)
├── internal/
│   ├── manifest/       # parse + validate the manifest (machine truth)
│   ├── runtime/        # tool CLI: flags, input validation, output contract
│   ├── build/          # manifest + skill -> one self-contained binary
│   ├── skill/          # skill validation and the no-schema linter (spec §30)
│   ├── daemon/         # socket server, invocation, registry, telemetry
│   ├── registry/       # on-disk discovery records (never a schema source)
│   ├── ipc/            # Unix-socket framing
│   ├── auth/           # credential resolution
│   ├── keychain/       # macOS Keychain integration
│   ├── browser/        # shared sessions / browser automation (stub)
│   ├── protocol/       # the Executor seam
│   │   ├── rest/       # REST executor (implemented)
│   │   ├── graphql/    # GraphQL executor (implemented)
│   │   └── grpc/       # stub
│   ├── mcp/            # MCP adapter over the same execution path
│   ├── telemetry/      # local, privacy-first usage events
│   ├── permissions/    # capability + permission classification and policy
│   ├── paths/          # the ~/.relay layout
│   └── fsutil/         # file copy / permission helpers
├── pkg/
│   ├── relay/          # public, language-neutral wire types + error model
│   └── toolruntime/    # the runtime embedded into every built tool
├── templates/          # manifest + SKILL.md starting points
├── examples/           # worked examples: github, slack, filesystem, stripe
├── skills/             # shared skill assets and conventions
├── docs/
│   ├── DESIGN.md       # the full design spec
│   └── CONTRIBUTING.md # how to add and verify a tool
├── scripts/e2e.sh      # end-to-end suite
├── Makefile
├── go.mod / go.sum
├── TASKS.md            # the phased build-out checklist
└── README.md
```

`dist/` holds built binaries and `~/.relay/` holds installed tools, the daemon
socket, the registry, and logs; both are generated, not part of the source.

## Authoring a tool

A tool is two files next to each other, `<tool>.yaml` and `SKILL.md`, built into
one binary:

- Start from [`templates/tool.yaml`](templates/tool.yaml) and
  [`templates/SKILL.md`](templates/SKILL.md).
- Read a worked example in [`examples/github/`](examples/github/github.yaml) —
  the canonical, spec-derived pair — or
  [`examples/slack/`](examples/slack/slack.yaml) for a REST service with real
  input schemas and authentication.
- See [`examples/filesystem/`](examples/filesystem/filesystem.yaml) for the
  local-capability shape (a non-REST tool; not yet executable).

The split is the whole point: the **manifest is machine truth** and answers
*what operations exist and what inputs they require*; the **skill is LLM
guidance** and answers *how an agent should use them*. A build-time linter
rejects a skill that restates the manifest's schemas, because two sources of
truth drift (spec §7, §30). The manifest and its skill are versioned together
(spec §36).

## Documentation

- [`docs/DESIGN.md`](docs/DESIGN.md) — the complete design spec.
- [`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md) — how to add and verify a tool.
- [`TASKS.md`](TASKS.md) — the phased build-out checklist with acceptance criteria.

## Status

The core loop works end to end: build a tool, inspect it, run the daemon,
install the tool, and execute the same capability through the CLI and through
MCP. REST and GraphQL have executors wired into the daemon; gRPC, browser, and
local capabilities are planned (see [TASKS.md](TASKS.md) and spec §23, §45–§46).
Credentials live in the macOS Keychain and are managed with `relay auth`;
`relay stats` reads the local telemetry stream. The `permissions` package
classifies capability and permission decisions, and the daemon enforces them
before it reads a credential or dispatches to an executor (spec §24, §25). A
manifest that declares neither capabilities nor permissions is treated as a
pre-model tool and runs unchecked.
