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

- [x] Go toolchain (1.22+) — installed at `/usr/local/go/bin/go` (Go 1.27.1)
- [x] `go mod tidy` to generate `go.sum` (the scaffold ships `go.mod` only)
- [x] `go build ./...` and `go test ./...` pass
- [ ] Decide the module path. The scaffold uses the local `relay`; move to `github.com/<you>/relay` before publishing. (open decision #1)

---

## M0 — Repository scaffold

**Goal:** a Go module that builds, with the layout from §4.

- [x] Layout: `cmd/{relay,relayd,relay-tool}`, `internal/*`, `pkg/relay`, `docs`, `examples`, `templates`, `skills`
- [x] `go.mod` (`module relay`, `gopkg.in/yaml.v3`)
- [x] Manifest types + validator (`internal/manifest`)
- [x] `protocol.Executor` seam (`internal/protocol`)
- [x] Public wire types + error codes (`pkg/relay`)
- [x] Stub entrypoints for `relay`, `relayd`, `relay-tool`
- [x] `go mod tidy`; commit `go.sum`
- [x] `gofmt` / `go vet` clean; add a `Makefile` (`build`, `test`, `fmt`, `vet`, `e2e`, `tidy`, `clean`)
- [x] CI: build + vet + test on push (`.github/workflows/ci.yml`)

**Acceptance:** `go build ./...` succeeds; `go run ./cmd/relay version` prints a version.

---

## M1 — Manifest + generated binary  (§6, §7, §9–§13, §52)

**Goal:** `relay build tool.yaml` produces one self-describing binary.

- [x] Complete manifest schema: `apiVersion`, `kind`, `metadata`, `runtime`, `protocol`, `auth`, `capabilities`, `permissions`, `tools[]` — `internal/manifest/manifest.go`
- [x] Validation rules (§59): required fields; duplicate operation names; unknown protocol; unknown auth type; path param `{x}` must be a `required` input property; `apiVersion`/`kind` correctness; aggregate *all* errors in one report — `internal/manifest/validate.go` + `validate_test.go`
- [x] Build pipeline (§12): parse → validate manifest → validate skill → package both → compile the generic runtime → embed → write `dist/<name>` — `internal/build/build.go`
- [x] Embedding (§8): manifest + skill via `go:embed` into a per-tool build; keep the runtime generic in `pkg/toolruntime` — `internal/build/toolmain.go.tmpl`
- [x] Binary contract (§9): `--describe` (JSON), `--skill`, `--help`, `--version` — `internal/runtime/runtime.go`
- [x] CLI generation (§11): `snake_case` op → `kebab-case` verb; flags derived from `input.properties`; plus `--json`, `--input <file>`, `--input-json <json>` — `internal/runtime/flags.go`
- [x] Input validation against the operation's JSON schema; structured `INVALID_INPUT` on failure
- [x] Output contract (§10): result → stdout, logs → stderr, exit codes (`0` ok, `1` error, `2` usage)
- [x] `relay build github.yaml --skill ./SKILL.md` — `cmd/relay/main.go`

**Implemented in:** `internal/manifest`, `internal/skill`, `internal/runtime`, `internal/build`, `pkg/toolruntime`; end-to-end covered by `scripts/e2e.sh`.

**Acceptance (§52, §60):**

- [x] `relay build examples/github/github.yaml --skill examples/github/SKILL.md` produces `dist/github` (3.07 MB, `-trimpath -ldflags "-s -w"`)
- [x] `./dist/github --describe` returns valid JSON matching §9
- [x] `./dist/github --skill` returns the embedded `SKILL.md`
- [x] `--help` and `--version` behave
- [x] the binary works moved to a clean directory with no YAML or source present

**Verified:** `make vet`, `go test ./...`, and `scripts/e2e.sh` (19/19) all pass at commit `729a62d`.

---

## M2 — Daemon, IPC, registry  (§13–§18, §35, §53)

**Goal:** move execution behind the Unix-socket daemon.

- [x] `relayd`: socket at `~/.relay/run/daemon.sock`; JSON frames; creates `~/.relay/{bin,cache,config,logs,registry,run,tools}` — `internal/daemon/server.go`, `internal/paths`, `cmd/relayd/main.go`
- [x] IPC protocol (§14): `invoke` request/response, version handshake, and a small frame set (`hello`, `invoke`, `list`, `inspect`, `status`, `register`, `remove`) — `internal/ipc/ipc.go`, `pkg/relay/ipc.go`
- [x] Registry (§15, §16): `relay install ./github` runs `--describe`, validates the descriptor, checks runtime compatibility, copies the binary to `~/.relay/tools/`, writes `~/.relay/registry/<name>.json`, and links the tool into `~/.relay/bin` (§38) — `internal/daemon/install.go`, `internal/registry`
- [x] Registry is discovery-only; the binary stays authoritative for schemas — the daemon re-reads `--manifest`/`--describe` from the installed binary on every invoke/inspect and stores no schema (`Installation` carries operations only as a display summary)
- [x] Runtime compatibility (§35): reject incompatible `runtime.apiVersion` with `RUNTIME_INCOMPATIBLE` — `pkg/relay/runtime.go`, `internal/daemon/tool.go` (`checkRuntime`, `validateDescriptor`)
- [x] Single-instance daemon (flock on `run/daemon.lock`); `relay daemon start|stop|restart|status|install`; graceful shutdown on SIGTERM — `internal/ipc/flock_unix.go`, `cmd/relay/main.go`
- [x] Tool runtime routes through the daemon (§13); the tool binary executes nothing itself — `pkg/toolruntime/toolruntime.go` dials the socket and sends `hello` + `invoke`
- [x] `relay list` and `relay inspect <tool>` read the registry (with an on-disk fallback when the daemon is stopped) — `cmd/relay/main.go`
- [x] `relay logs` tails the daemon log

**Implemented in:** `internal/{ipc,paths,registry,fsutil,daemon}`, `pkg/relay/ipc.go`, `pkg/toolruntime`, `cmd/relayd`, `cmd/relay`.

**Decisions taken:**

- **IPC framing — newline-delimited JSON, no correlation IDs.** Callers are short-lived processes making a handful of sequential calls over one connection; pipelining would add correlation bookkeeping without buying anything. Open decision #4 is resolved for v1.
- **One lock per Relay home, not per socket.** `--socket` moves where the daemon listens but not which home it owns, because the registry has exactly one writer. Installing a second daemon for one home fails cleanly rather than racing.
- **Daemon re-validates input.** A tool binary is treated as untrusted, so the daemon re-runs the same schema validation the tool already did (§40).
- **Only the copied binary and the PATH link are removed** by `relay uninstall`; the source a user installed from is never touched.

**Acceptance (§53):**

- [x] `relay install ./dist/github` registers the tool (registry record + `~/.relay/tools/github` + `~/.relay/bin/github` symlink)
- [x] `github get-repository --owner X --repo Y` routes through the daemon; with no executor for `rest` yet it fails `PROTOCOL_ERROR` after passing schema validation (M3 adds the executor)
- [x] The registry survives a daemon restart — it is plain JSON on disk, re-read at startup
- [x] A second daemon on the same home exits 1 with a clean message; the first keeps serving

**Verified:** `gofmt -l .` empty, `go build ./...`, `go vet ./...`, `go test ./...`, and `scripts/e2e.sh` (37/37) all pass.

---



---

## M3 — REST executor  (§19, §20, §26)

**Goal:** REST is the first protocol, behind `Executor`.

- [x] `RESTExecutor`: GET/POST/PUT/PATCH/DELETE; path templating from input; query params; headers; JSON body; JSON response
- [x] Method, path, query, headers, and body declared in the manifest; response mapped to the JSON result
- [x] Response body is parsed as JSON when the content type says so, with a raw fallback
- [x] Structured errors (§26): map status/transport failures → `AUTH_REQUIRED`, `AUTH_FAILED`, `PERMISSION_DENIED`, `RATE_LIMITED`, `REMOTE_ERROR`, `NETWORK_ERROR`, `TIMEOUT`
- [x] Context timeouts and cancellation; retry/backoff for 429/5xx honoring `Retry-After`, bounded attempts
- [ ] Pagination (§20): a declared strategy (Link header and cursor param at minimum)
      Deferred on purpose: the manifest has no pagination block yet, so there is
      nothing for a tool to declare. The executor performs one request; Link-header
      and cursor strategies land together with the manifest field that expresses them.
- [x] Protocol dispatch from `protocol.type`; unknown or unimplemented type → `PROTOCOL_ERROR`

**Notes.** Retries are limited to idempotent methods (GET/HEAD/PUT/DELETE); a POST is
never replayed, because a duplicate write is worse than a failed one. `Retry-After`
is honoured in its delay-seconds form (capped); the HTTP-date form is not parsed yet.
Path placeholders are `url.PathEscape`d, while query and header values are substituted
raw so `url.Values.Encode` owns query encoding.

**Implemented in:** `internal/protocol/rest`, dispatched from `internal/daemon`.

**Acceptance:** `github get_repository` and `list_pull_requests` work against GitHub (or a mock); the error taxonomy is verified against mocked 401/403/404/429/500.

---

## M4 — Auth + Keychain  (§21, §22, §40, §54)

**Goal:** credentials live in the daemon and only in Keychain.

- [x] Credential types: API key, bearer token, basic auth, client credentials, OAuth2 — `internal/auth`
- [x] macOS Keychain via the `security` CLI (or Security.framework); namespace `com.relay.<tool>`, account `default` — `internal/keychain`
- [x] `relay auth login <tool>` / `logout` / `status` — the CLI hands the secret to the daemon once and never stores or echoes it
- [ ] OAuth2 device-code or PKCE flow — `relay auth login` stores an already-issued token; the flow choice is still open decision #5
- [x] Daemon injects credentials at execution time (§22); the tool binary never handles secrets — resolved through `auth.Resolver`
- [x] Redaction guarantees: secrets absent from CLI output, MCP output, logs, telemetry, manifests, skills — `internal/keychain/redact.go`
- [x] `AUTH_REQUIRED` / `AUTH_FAILED` surfaced identically to CLI and MCP

**Implemented in:** `internal/auth`, `internal/keychain`, `internal/daemon/auth.go`, `cmd/relay/main.go`.

**Decisions taken:**

- **Five credential types, one presentation table.** Each declared type maps to a conventional header: `api_key` to `X-API-Key`, `bearer` and `oauth2` to `Authorization: Bearer`, and `basic` plus `client_credentials` to HTTP Basic. No executor branches on the auth type, so the protocol stays an implementation detail (§19).
- **The CLI never stores a secret.** `relay auth login` reads it with echo disabled (or from stdin when unattended) and hands it to the daemon, the only writer of Keychain. There is deliberately no `--secret` flag, because argv is visible to `ps`.

**Verified:** an isolated `RELAY_HOME` round-trip — `relay auth login github` wrote `com.relay.github` to Keychain, `relay auth status` reported `authType: oauth2`, `relay auth logout` removed it, and `security find-generic-password -s com.relay.github` then reported the item missing. Grepping the temp home for the planted secret found nothing.

**Acceptance:** a secret stored in Keychain is used on invoke; a missing credential yields `AUTH_REQUIRED`; `relay logs` and telemetry contain no secret; grepping every artifact finds no token.

---

## M5 — Embedded skills  (§7, §8, §30, §36, §55)

**Goal:** every binary carries AI guidance and can serve it.

- [x] `SKILL.md` embedded alongside the manifest at build time (§8) — `internal/build`
- [ ] Versioned together with the manifest (§36) — the manifest carries `metadata.version`, but the skill has no version field, so a mismatch cannot fail the build yet
- [x] `--skill` prints it — `internal/runtime`
- [ ] The daemon serves the skill over IPC — the daemon records only a `Skill bool`; there is no `skill` IPC frame, so MCP cannot yet see the skill text (§29)
- [x] Skill linter: rejects skills that restate the input schema and requires workflow guidance — `internal/skill/lint.go`
- [x] `templates/SKILL.md` starting point

**Implemented in:** `internal/skill`, `internal/build` (lint gate), `internal/runtime`.
**Acceptance:** `github --skill` prints embedded guidance; the daemon returns identical text; a manifest/skill version mismatch fails the build.
The first clause passes; the last two are open — see the unchecked items above.

---

## M6 — MCP adapter  (§27, §28, §29, §41, §56)

**Goal:** expose everything to LLM clients with zero new execution logic.

- [x] `relay mcp` runs an MCP server over stdio; it exits 0 at stdin EOF — `internal/mcp`
- [x] Discovery: registry → tool descriptors → MCP tool list, exposing name, description, and input JSON schema
- [x] Naming: `github_get_repository` (the flat `<tool>_<operation>` form, §28)
- [x] Invocation: MCP call → the same daemon path as the CLI; no separate engine
- [ ] Skill exposure to MCP consumers (§29) — blocked on the same missing `skill` IPC frame as M5; the daemon serves no skill text yet
- [x] Errors use the exact same structured codes as the CLI — a missing credential returns `AUTH_REQUIRED` through both paths
- [x] Security: MCP inherits the permission and auth path; no MCP-only credential route (§41)

**Implemented in:** `internal/mcp`, wired by `cmd/relay`.

**Verified by hand:** `initialize` returns `protocolVersion` `2025-03-26`; `tools/list` includes `github_get_repository` with its `inputSchema`; a `tools/call` with no stored credential returns `result.isError: true` whose `content[0].text` carries the same `AUTH_REQUIRED` code the CLI reports; the server exits cleanly on stdin EOF.

**Acceptance (§60):** the same capability returns the same result through CLI and MCP; a parity test asserts `CLI definition == MCP definition`.
The parity assertions land in e2e section 11 (M8); the skill half stays open with §29 above.

---

## M7 — CLI UX, PATH, LaunchAgent  (§37, §38, §39, §42)

**Goal:** zero-friction install and use.

- [x] `relay build | install | list | inspect | daemon | logs | mcp | auth | stats | version` — every command in the spec's list exists, plus `stats`; there is no `uninstall`
- [x] Tools live under `~/.relay/tools/`; `~/.relay/bin` is linked on install (§38)
- [x] `relay daemon install` writes `~/Library/LaunchAgents/com.relay.daemon.plist` and prints the matching `launchctl load` line; it does not load the agent for you (§39)
- [x] `relay daemon start|stop|restart|status`; logs to `~/.relay/logs/`
- [ ] `brew` formula (packaging, post-MVP)

**Acceptance:** on a fresh Mac, install relay → the daemon auto-starts → `relay install ./github` → `github ...` works from any directory.

---

## M8 — End-to-end + testing  (§31, §59, §60, §61)

**Goal:** prove the core loop and lock it with tests.

- [x] Unit — manifest: required fields, duplicate operations, invalid protocol, invalid auth — `internal/manifest/validate_test.go`, `internal/manifest/input_test.go`
- [x] Unit — runtime: CLI parsing, input validation, JSON output, exit codes — `internal/runtime/runtime_test.go`
- [x] Unit — daemon: registration, invocation, IPC, registry, errors — `internal/daemon`, `internal/registry`
- [x] Unit — protocol: REST against `httptest` — `internal/protocol/rest/rest_test.go`
- [x] Security: unauthorized tool access, missing credentials, permission violations, credential leakage, MCP permission bypass — `internal/keychain/security_test.go`, `internal/daemon/security_test.go`, `internal/mcp/security_test.go`
- [x] E2E script (§60): build → `--describe` → install → invoke → `--skill` → MCP discovery → MCP invoke → assert CLI/MCP parity — `scripts/e2e.sh`
- [x] Emit a local usage event on each invocation (§31)
- [x] `make e2e` runs in CI — `.github/workflows/ci.yml` runs `bash scripts/e2e.sh` on push and pull request

**Acceptance (§61):** the 10-step Definition of Done below passes on a clean machine.

---

### MVP line

Everything above is the first release (§51). Everything below is post-MVP.

---

## M9 — Telemetry  (§31, §32, §33, §57)

- [x] Local, aggregated events under `~/.relay/telemetry/`: tool, operation, timestamp, durationMs, success
- [x] Never collect tokens, keys, request bodies, responses, or personal data
- [x] Per-tool summaries: operation frequency, sequences, failure rate, latency, rate limits
- [ ] Sequence mining → skill suggestions; proposals are reviewed, never auto-applied
      Sequences are mined and rendered by `relay stats`, but Relay does not yet
      draft a skill edit. The events a suggestion would be built from are already
      recorded, so this is purely additive.
- [ ] Optional anonymous opt-in — deliberately absent: telemetry is local only and
      there is no network path to opt into (spec §32).

**Implemented in:** `internal/telemetry` (recorder + summary), wired into the daemon
at `internal/daemon/telemetry.go` and surfaced by `relay stats`.

**Decisions taken:**

- **Telemetry is on by default and local.** The value comes from seeing how
  capabilities are really used, and the data never leaves the machine, so the
  daemon records by default and `relayd --no-telemetry` turns it off.
- **The event surface is the privacy guarantee.** `telemetry.Event` has no field
  for inputs, headers, bodies, or responses, so a secret cannot be recorded by
  accident. `TestUsageEventCarriesNoPayload` pins the wire shape so a new field
  cannot be added silently.
- **Recording can never fail an invocation.** A telemetry error is discarded, and
  the e2e suite asserts the on-disk stream contains no request input.

**Acceptance:** `relay stats` renders per-tool frequency, failure rate, latency, and
observed operation sequences from the local stream; grepping that stream for a
request input value finds nothing.

---

## M10 — Additional protocols  (§19, §23, §44, §45, §46, §58)

- [x] `GraphQLExecutor` (`protocol.type: graphql`) — query and variables from the manifest; CLI and MCP unchanged — `internal/protocol/graphql`
- [ ] `GRPCExecutor` (`protocol.type: grpc`) — service and method from the manifest; reflection or bundled descriptors
- [ ] `LocalExecutor` — filesystem, git, docker, kubectl, ssh, clipboard, notifications, calendar (§46)
- [ ] `BrowserExecutor` (§23) — shared login, cookies, sessions, OAuth, web interaction

---

## M11 — Security, permissions, distribution  (§24, §25, §40, §41, §47, §48, §49)

- [x] Capability declarations: `network`, `keychain`, `browser`, `filesystem.read`, `filesystem.write`, `shell`, `notifications`, `clipboard` — `internal/permissions`
- [x] Permission policy: host allowlists, filesystem scopes; a tool asking for a new capability is not silently granted it — `internal/permissions/policy.go`, enforced in `internal/daemon/permissions.go`
- [x] Confirmation policy for destructive operations (`delete_repository`, `send_message`, `charge_customer`) with CLI and MCP parity — `CodeConfirmationRequired` → `PERMISSION_DENIED` on both paths
- [ ] `relayd --destructive-op` (or a config file) so the daemon's destructive list is set without a code change — `Config.DestructiveOps` exists but nothing populates it
- [ ] Interactive confirmation prompt for destructive operations — today a matched operation is refused outright
- [ ] Signed tools: verify publisher, signature, binary, and manifest at install (§48)
- [ ] Trust levels: trusted / verified / unknown / blocked (§49)
- [ ] Hosted registry, `relay search`, `relay install <name>` (§47)

**Decisions taken:**

- **The capability model is opt-in per tool.** `participatesInCapabilityModel` (`internal/daemon/permissions.go`) is true only when a manifest declares `capabilities` or `permissions`. A pre-model manifest states no requirements and runs unchecked, so the check could not silently break tools built before it existed; a manifest that declares either is then held default-deny — an unnamed capability is refused and an unscoped host is refused (§24). The destructive gate sits outside this opt-out on purpose: it is driven by daemon configuration, so a destructive operation is gated whether or not the tool declares a capability surface.
- **The destructive list is daemon-owned, not manifest-owned.** `PolicyFromManifest` takes it from the caller (`Config.DestructiveOps`), so a tool cannot widen its own confirmation surface by editing its manifest.
- **A destructive operation is refused, not prompted.** There is no interactive confirmation yet, so a matched operation returns `PERMISSION_DENIED` naming confirmation rather than running.

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
8. **Query/body templating** — RESOLVED in M3. Path, query, and header values all
   substitute `{param}` from the operation input. Path segments are `url.PathEscape`d
   (so a value cannot rewrite the path), while query and header values are substituted
   raw and then encoded by `url.Values.Encode`. A requested query parameter whose input
   is absent is omitted rather than sent empty.

## Risks

- **Keychain friction** — the `security` CLI prompts and ACLs can stall automation; validate early in M4.
- **Generic runtime vs per-endpoint codegen** — resist generating endpoint-specific Go; one consistent runtime is the whole product (§13).
- **MCP schema fidelity** — JSON Schema and MCP differ slightly; assert CLI/MCP parity in M6.
- **LaunchAgent restarts** — socket and lock handling must survive crashes; test in M7.
- **Skill/schema drift** — enforce paired versioning (§36) and the no-schema-in-skill lint (§30).
