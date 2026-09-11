# Relay

**The local capability runtime for AI agents.**

Relay turns external services and local capabilities into self-describing,
AI-native tools. A Relay tool is a single portable executable carrying three
things:

1. a machine-readable **manifest** (what it can do),
2. an embedded **SKILL.md** (how an AI should use it), and
3. a generic **runtime** that executes the declared operations.

A long-running **daemon** provides the shared infrastructure — auth, Keychain,
sessions, browser, HTTP transport, permissions, registry, telemetry, and MCP.

```
Tool = executable + manifest + skill
The daemon is the runtime.
The manifest is machine truth.
The skill is LLM guidance.
MCP is an adapter.
The underlying protocol is an implementation detail.
```

## Why Go

The product is a collection of portable executables. Go gives single-binary
distribution, trivial cross-compilation, strong HTTP and concurrency support,
Unix sockets, and easy embedding.

The **manifest and CLI contracts are the product**; the implementation language
is not. Keep Relay's public model free of Go-specific assumptions.

## Status

Early scaffold. Nothing here executes a real capability yet — the manifest
types, the protocol seam, and the public wire/error contracts are in place, and
the rest is laid out in [TASKS.md](TASKS.md).

## Repository layout

```
relay/
├── cmd/
│   ├── relay/       # developer/user CLI
│   ├── relayd/      # the daemon
│   └── relay-tool/  # generic tool runtime (the embedded runtime)
├── internal/
│   ├── manifest/    # parse + validate the manifest (machine truth)
│   ├── runtime/     # generic tool runtime: CLI parsing, input validation
│   ├── daemon/      # socket server, invocation, lifecycle
│   ├── registry/    # discovery metadata over installed tools
│   ├── ipc/         # Unix-socket framing and transport
│   ├── auth/        # credential resolution
│   ├── keychain/    # macOS Keychain integration
│   ├── browser/     # shared sessions / browser automation
│   ├── protocol/    # Executor seam: rest/, graphql/, grpc/
│   ├── mcp/         # MCP adapter over the same execution path
│   ├── telemetry/   # local, privacy-first usage events
│   └── permissions/ # capabilities and policy enforcement
├── pkg/relay/       # public, language-neutral wire types and error model
├── templates/       # manifest + SKILL.md starting points
├── examples/        # example tool manifests
├── skills/          # reusable skill assets
└── docs/DESIGN.md   # the full design spec
```

## Build

Requires the Go toolchain (1.22+).

```sh
go mod tidy          # generate go.sum
go build ./...
go test ./...

go run ./cmd/relay version
```

> The module path is currently the local `relay`. Switch it to
> `github.com/<you>/relay` before publishing.

## Documentation

- [docs/DESIGN.md](docs/DESIGN.md) — the complete design spec.
- [TASKS.md](TASKS.md) — the phased build-out checklist with acceptance criteria.
