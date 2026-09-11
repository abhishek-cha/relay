# Relay — Build Tasks

Implementation checklist for the Relay MVP described in [docs/DESIGN.md](docs/DESIGN.md).
Work top to bottom; every milestone is independently verifiable.

## How to read this

- `[x]` done · `[ ]` open · `[~]` in progress
- Each milestone has a **Goal**, **Tasks**, and **Acceptance** section.
- Spec refs point at numbered sections of `docs/DESIGN.md`.
- The MVP line falls after **M8**. M9+ is post-MVP.

## Non-negotiable invariants

Break any of these and the product stops being Relay.

1. **Tool = executable + manifest + skill.** A built binary carries all three.
2. **The daemon is the runtime.** Tool binaries never talk to a service directly.
3. **The manifest is machine truth.** The registry is discovery metadata only, never a schema source.
4. **The skill is LLM guidance, not a second schema.** No input/output schemas in `SKILL.md`.
5. **MCP is an adapter, not an execution engine.** CLI and MCP share one code path.
6. **Credentials are daemon-owned.** They never reach CLI output, MCP output, logs, telemetry, manifests, or skills.
7. **`stdout` = machine result, `stderr` = diagnostics, exit code = status.** Logs never mix into JSON stdout.
8. **The protocol is an implementation detail.** REST/GraphQL/gRPC/browser sit behind one `Executor` interface.

## Prerequisites

- [ ] Go toolchain (1.22+) — **not installed on this machine** (`go: command not found`); `brew install go`
- [ ] `go mod tidy` to generate `go.sum` (the scaffold ships `go.mod` only)
- [ ] `go build ./...` and `go test ./...` pass
- [ ] Decide the module path. The scaffold uses the local `relay`; move to `github.com/<you>/relay` before publishing.

---

## M0 — Repository scaffold

**Goal:** a Go module that builds, with the layout from §4.

- [x] Layout: `cmd/{relay,relayd,relay-tool}`, `internal/*`, `pkg/relay`, `docs`, `examples`, `templates`, `skills`
- [x] `go.mod` (`module relay`, `gopkg.in/yaml.v3`)
- [x] Manifest types + validator (`internal/manifest`)
- [x] `protocol.Executor` seam (`internal/protocol`)
- [x] Public wire types + error codes (`pkg/relay`)
- [x] Stub entrypoints for `relay`, `relayd`, `relay-tool`
- [ ] `go mod tidy`; commit `go.sum`
- [ ] `gofmt` / `go vet` clean; add a `Makefile` (`build`, `test`, `fmt`, `lint`)
- [ ] CI: build + vet + test on push

**Acceptance:** `go build ./...` succeeds; `go run ./cmd/relay version` prints a version.

---

## M1 — Manifest + generated binary  (§6, §7, §9–§13, §52)

**Goal:** `relay build tool.yaml` produces one self-describing binary.

- [ ] Complete manifest schema: `apiVersion`, `kind`, `metadata`, `runtime`, `protocol`, `auth`, `capabilities`, `permissions`, `tools[]`
- [ ] Validation rules (§59): required fields; duplicate operation names; unknown protocol; unknown auth type; path param `{x}` must be a `required` input property; `apiVersion`/`kind` correctness; aggregate *all* errors in one report
- [ ] Build pipeline (§12): parse → validate manifest → validate skill → package both → compile the generic runtime → embed → write `dist/<name>`
- [ ] Embedding (§8): manifest + skill via `go:embed` into a per-tool build; keep the runtime generic in `cmd/relay-tool`
- [ ] Binary contract (§9): `--describe` (JSON), `--skill`, `--help`, `--version`
- [ ] CLI generation (§11): `snake_case` op → `kebab-case` verb; flags derived from `input.properties`; plus `--json`, `--input <file>`, `--input-json <json>`
- [ ] Input validation against the operation's JSON schema; structured `INVALID_INPUT` on failure
- [ ] Output contract (§10): result → stdout, logs → stderr, exit codes
- [ ] `relay build github.yaml --skill ./SKILL.md`

**Acceptance (§52, §60):**

- `relay build examples/github/github.yaml --skill examples/github/SKILL.md` produces `dist/github`
- `./dist/github --describe` returns valid JSON matching §9
- `./dist/github --skill` returns the embedded `SKILL.md`
- `--help` and `--version` behave
- the binary works moved to a clean directory with no YAML or source present

---

## M2 — Daemon, IPC, registry  (§13–§18, §35, §53)

**Goal:** move execution behind the Unix-socket daemon.

- [ ] `relayd`: socket at `~/.relay/run/daemon.sock`; JSON frames; create `~/.relay/{config,registry,run,logs,cache}`
- [ ] IPC protocol (§14): `invoke` request/response, correlation IDs, version handshake; streaming not required for v1
- [ ] Registry (§15, §16): `relay install ./github` runs `--describe`, validates the descriptor, checks runtime compatibility, copies the binary to `~/.relay/tools/`, writes `~/.relay/registry/<name>.json`
- [ ] Registry is discovery-only; the binary stays authoritative for schemas — never serve a cached schema
- [ ] Runtime compatibility (§35): reject incompatible `runtime.apiVersion` with `RUNTIME_INCOMPATIBLE`
- [ ] Single-instance daemon (lock file); `relay daemon start|stop|status`; graceful shutdown on SIGTERM
- [ ] Tool runtime routes through the daemon (§13); the tool binary executes nothing itself
- [ ] `relay list` and `relay inspect <tool>` read the registry

**Acceptance:** `relay install ./dist/github` registers the tool; `github get-repository --owner X --repo Y` routes through the daemon; a daemon restart preserves the registry.

---

## M3 — REST executor  (§19, §20, §26)

**Goal:** REST is the first protocol, behind `Executor`.

- [ ] `RESTExecutor`: GET/POST/PUT/PATCH/DELETE; path templating from input; query params; headers; JSON body; JSON response
- [ ] Method, path, query, headers, and body declared in the manifest; response mapped to the JSON result
- [ ] Response body is parsed as JSON when the content type says so, with a raw fallback
- [ ] Structured errors (§26): map status/transport failures → `AUTH_REQUIRED`, `AUTH_FAILED`, `PERMISSION_DENIED`, `RATE_LIMITED`, `REMOTE_ERROR`, `NETWORK_ERROR`, `TIMEOUT`
- [ ] Context timeouts and cancellation; retry/backoff for 429/5xx honoring `Retry-After`, bounded attempts
- [ ] Pagination (§20): a declared strategy (Link header and cursor param at minimum)
- [ ] Protocol dispatch from `protocol.type`; unknown or unimplemented type → `PROTOCOL_ERROR`

**Acceptance:** `github get_repository` and `list_pull_requests` work against GitHub (or a mock); the error taxonomy is verified against mocked 401/403/404/429/500.

---

## M4 — Auth + Keychain  (§21, §22, §40, §54)

**Goal:** credentials live in the daemon and only in Keychain.

- [ ] Credential types: API key, bearer token, basic auth, client credentials, OAuth2
- [ ] macOS Keychain via the `security` CLI (or Security.framework); namespace `com.relay.<tool>`, account `default`
- [ ] `relay auth login <tool>` / `logout` / `status`; OAuth2 device-code or PKCE foundation
- [ ] Daemon injects credentials at execution time (§22); the tool binary never handles secrets
- [ ] Redaction guarantees: secrets absent from CLI output, MCP output, logs, telemetry, manifests, skills
- [ ] `AUTH_REQUIRED` / `AUTH_FAILED` surfaced identically to CLI and MCP

**Acceptance:** a secret stored in Keychain is used on invoke; a missing credential yields `AUTH_REQUIRED`; `relay logs` and telemetry contain no secret; grepping every artifact finds no token.

---

## M5 — Embedded skills  (§7, §8, §30, §36, §55)

**Goal:** every binary carries AI guidance and can serve it.

- [ ] `SKILL.md` embedded alongside the manifest at build time; versioned together with it (§36)
- [ ] `--skill` prints it; the daemon can serve it over IPC
- [ ] Skill linter: reject skills that restate the input schema (§30); require workflow/sequence guidance
- [ ] `templates/SKILL.md` starting point

**Acceptance:** `github --skill` prints embedded guidance; the daemon returns identical text; a manifest/skill version mismatch fails the build.

---

## M6 — MCP adapter  (§27, §28, §29, §41, §56)

**Goal:** expose everything to LLM clients with zero new execution logic.

- [ ] `relay mcp` runs an MCP server (stdio first)
- [ ] Discovery: registry → tool descriptors → MCP tool list, exposing name, description, and input JSON schema
- [ ] Naming: `github_get_repository` (or the namespace form when the client supports it, §28)
- [ ] Invocation: MCP call → the same daemon path as the CLI; no separate engine
- [ ] Skill exposure to MCP consumers (§29)
- [ ] Errors use the exact same structured codes as the CLI
- [ ] Security: MCP inherits the permission and auth path; no MCP-only credential route (§41)

**Acceptance (§60):** the same capability returns the same result through CLI and MCP; a parity test asserts `CLI definition == MCP definition`.

---

## M7 — CLI UX, PATH, LaunchAgent  (§37, §38, §39, §42)

**Goal:** zero-friction install and use.

- [ ] `relay build | install | list | inspect | daemon | logs | mcp | auth | version`
- [ ] Tools live under `~/.relay/tools/`; optional `~/.relay/bin` on `PATH`
- [ ] `relay daemon install` writes `~/Library/LaunchAgents/com.relay.daemon.plist`; auto-start at login (§39)
- [ ] `relay daemon start|stop|restart|status`; logs to `~/.relay/logs/`
- [ ] `brew` formula (packaging, post-MVP)

**Acceptance:** on a fresh Mac, install relay → the daemon auto-starts → `relay install ./github` → `github ...` works from any directory.

---

## M8 — End-to-end + testing  (§31, §59, §60, §61)

**Goal:** prove the core loop and lock it with tests.

- [ ] Unit — manifest: required fields, duplicate operations, invalid protocol, invalid auth
- [ ] Unit — runtime: CLI parsing, input validation, JSON output, exit codes
- [ ] Unit — daemon: registration, invocation, IPC, registry, errors
- [ ] Unit — protocol: REST against `httptest`
- [ ] Security: unauthorized tool access, missing credentials, permission violations, credential leakage, MCP permission bypass
- [ ] E2E script (§60): build → `--describe` → install → invoke → `--skill` → MCP discovery → MCP invoke → assert CLI/MCP parity
- [ ] Emit a local usage event on each invocation (§31)
- [ ] `make e2e` runs in CI

**Acceptance (§61):** the 10-step Definition of Done below passes on a clean machine.

---

### MVP line

Everything above is the first release (§51). Everything below is post-MVP.

---

## M9 — Telemetry  (§31, §32, §33, §57)

- [ ] Local, aggregated events under `~/.relay/telemetry/`: tool, operation, timestamp, durationMs, success
- [ ] Never collect tokens, keys, request bodies, responses, or personal data
- [ ] Per-tool summaries: operation frequency, sequences, failure rate, latency, rate limits
- [ ] Sequence mining → skill suggestions; proposals are reviewed, never auto-applied
- [ ] Optional anonymous opt-in

---

## M10 — Additional protocols  (§19, §23, §44, §45, §46, §58)

- [ ] `GraphQLExecutor` (`protocol.type: graphql`) — query and variables from the manifest; CLI and MCP unchanged
- [ ] `GRPCExecutor` (`protocol.type: grpc`) — service and method from the manifest; reflection or bundled descriptors
- [ ] `LocalExecutor` — filesystem, git, docker, kubectl, ssh, clipboard, notifications, calendar (§46)
- [ ] `BrowserExecutor` (§23) — shared login, cookies, sessions, OAuth, web interaction

---

## M11 — Security, permissions, distribution  (§24, §25, §40, §41, §47, §48, §49)

- [ ] Capability declarations: `network`, `keychain`, `browser`, `filesystem.read`, `filesystem.write`, `shell`, `notifications`, `clipboard`
- [ ] Permission policy: host allowlists, filesystem scopes; a tool asking for a new capability is not silently granted it
- [ ] Confirmation policy for destructive operations (`delete_repository`, `send_message`, `charge_customer`) with CLI and MCP parity
- [ ] Signed tools: verify publisher, signature, binary, and manifest at install (§48)
- [ ] Trust levels: trusted / verified / unknown / blocked (§49)
- [ ] Hosted registry, `relay search`, `relay install <name>` (§47)

---

## Definition of Done (§61)

Relay MVP is complete when a developer can:

1. Define a service — `github.yaml`
2. Define its AI guidance — `SKILL.md`
3. Build one binary — `relay build github.yaml`
4. Distribute it — `github`
5. Install it — `relay install github`
6. Inspect it — `github --describe`, `github --skill`
7. Execute it — `github get-repository ...`
8. Expose it to an LLM — Relay MCP
9. Execute the same capability through MCP — `LLM → MCP → Relay → github → API`
10. Observe usage — Relay locally records capability usage patterns

## Non-goals for the MVP (§2)

Cloud control plane · hosted registry · marketplace · complex agent
orchestration · distributed daemon · remote execution · LLM-generated skills ·
GraphQL and gRPC alongside REST · complex GUI · multi-user permissions.

## Open decisions

1. **Module path** — keep `relay` or move to a GitHub path before publishing.
2. **Daemon language boundary** — Go now; the manifest and CLI contracts must stay language-neutral.
3. **MCP transport** — stdio first; add a socket/TCP transport only if a client needs it.
4. **IPC framing** — newline-delimited JSON is fine for v1; revisit if streaming or binary payloads appear.
5. **OAuth flow** — device code vs PKCE vs browser handoff; decide in M4 and reuse the browser capability later.
6. **Config format** — pick TOML, YAML, or JSON for `~/.relay/config` in M7.
7. **Binary size** — generic runtime plus embedded assets; confirm a few MB is acceptable.
8. **Query/body templating** — the draft `slack` and `stripe` examples bind query values with `{param}`; confirm in M3 that path, query, headers, and body share one substitution convention.

## Risks

- **Keychain friction** — the `security` CLI prompts and ACLs can stall automation; validate early in M4.
- **Generic runtime vs per-endpoint codegen** — resist generating endpoint-specific Go; one consistent runtime is the whole product (§13).
- **MCP schema fidelity** — JSON Schema and MCP differ slightly; assert CLI/MCP parity in M6.
- **LaunchAgent restarts** — socket and lock handling must survive crashes; test in M7.
- **Skill/schema drift** — enforce paired versioning (§36) and the no-schema-in-skill lint (§30).
