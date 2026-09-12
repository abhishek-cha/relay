# Contributing a Tool

A Relay tool is two files authored side by side, then compiled into one portable
binary:

```
mytool/
├── mytool.yaml   # the manifest — machine truth (spec §6)
└── SKILL.md      # the skill — LLM guidance (spec §7, §30)
```

Start from [`templates/tool.yaml`](../templates/tool.yaml) and
[`templates/SKILL.md`](../templates/SKILL.md), and read a worked example before
writing your own: [`examples/github/`](../examples/github/github.yaml) is the
canonical pair, and [`examples/slack/`](../examples/slack/slack.yaml) shows a
REST service with several operations, real input schemas, and authentication.

## The manifest is machine truth

The manifest declares what operations exist and what inputs they require. It is
validated at build time, and the validator reports every problem at once rather
than failing on the first (spec §59). The rules:

- `apiVersion: relay/v1` and `kind: Tool` are required and exact.
- `metadata.name` is lowercase snake_case; `metadata.version` is required.
- `protocol.type` is one of `rest`, `graphql`, `grpc`, `browser`.
  `baseUrl` is required for `rest` and `endpoint` for `graphql` (spec §19).
- `auth` is optional; when present its `type` is one of `api_key`, `bearer`,
  `basic`, `oauth2`, `client_credentials`. The manifest states the requirement;
  the daemon owns the secret and keeps it in the Keychain (spec §21, §22).
- Every entry in `tools[]` needs a snake_case `name`, a `description`, an
  `input` schema of `type: object`, and a `request`. For `rest` (and the other
  HTTP-shaped protocols) the `request` needs `method` and `path`; for `graphql`
  it needs a `document`. Duplicate operation names are rejected.
- For `rest`, a `{param}` in `request.path` must name a declared input property,
  and that property must be listed in `input.required`.
- `capabilities` draws from the known set (`network`, `keychain`, `browser`,
  `filesystem.read`, `filesystem.write`, `shell`, `notifications`,
  `clipboard`), and `permissions` narrows them — network hosts, filesystem
  scopes (spec §24, §25).

For REST, an operation's inputs become the request deterministically: path
placeholders from `request.path`, query parameters from `request.query`, headers
from `request.headers`, and — for methods that carry a body — any remaining
inputs are sent as a JSON body. That last rule is why a POST operation usually
needs no explicit `body` block.

## The skill is guidance, not a second schema

`SKILL.md` answers a different question: *how should an agent use these
operations effectively?* Cover usage patterns, workflow sequences, constraints,
choosing between operations, common mistakes, pagination, and interpreting
results (spec §30). Never restate the manifest's inputs or outputs.

A build-time linter enforces the split, and it is anchored to the names your
manifest declares, so a finding is always explainable:

- **Fails the build** — a fenced `yaml`/`yml`/`json` block that mirrors the
  manifest (contains a schema key such as `input` or `required`, or a declared
  operation name); a `name: type` line naming a declared input property; or a
  `Parameters`/`Inputs`/`Arguments` section that lists declared fields.
- **Warns (build still succeeds)** — no workflow guidance at all (add an arrow
  sequence, a numbered workflow, or a `When to use` rule); or a backticked
  snake_case token that names neither a declared operation nor a declared input.

The examples show the style. [`examples/slack/SKILL.md`](../examples/slack/SKILL.md)
also demonstrates the honest note worth writing when a tool carries a caveat an
agent must know.

## Build and verify

```sh
export PATH="$PATH:/usr/local/go/bin"
cd /path/to/relay

# Build the CLI and daemon once, then put them on PATH for this shell.
make build                       # -> dist/relay, dist/relayd
export PATH="$PWD/dist:$PATH"

# Build one binary from the pair.
relay build examples/slack/slack.yaml --skill examples/slack/SKILL.md

# Inspect the binary's contract without a daemon.
./dist/slack --describe
./dist/slack --skill
```

The build fails with a clear message if the manifest is invalid or the skill
restates the schema. A deliberately schema-restating skill is the fastest way to
confirm the linter is active — it should fail with a `spec §30` error.

## Install and run

```sh
export PATH="$PATH:/usr/local/go/bin"
cd /path/to/relay
make build
export PATH="$PWD/dist:$PATH"         # relay, relayd
export PATH="$HOME/.relay/bin:$PATH"  # installed tools, by bare name

relay daemon start
relay install ./dist/slack
relay list

# slack declares bearer auth, so it needs a stored credential first (spec §21).
# The prompt reads the secret with echo disabled and never puts it in argv.
relay auth login slack
slack auth-test
```

Installation copies the binary into `~/.relay/tools`, writes a discovery record
in `~/.relay/registry`, and links the tool into `~/.relay/bin` (spec §15, §38).
Relay never edits your shell configuration, so `~/.relay/bin` has to be on
`PATH` for a tool to run by bare name. The registry is discovery metadata only:
the installed binary stays the authoritative schema source, re-read on every
invocation (spec §16).

## Expose it to an MCP client

There is nothing extra to build. `relay mcp` lists every registered tool over
stdio and routes each call through the same daemon path as the CLI (spec §27,
§41):

```json
{
  "mcpServers": {
    "relay": { "command": "relay", "args": ["mcp"] }
  }
}
```

## A note on the browser example

Not every service has an API worth wrapping. [`examples/browser/`](../examples/browser/browser.yaml)
models one that is only reachable through a login form: Relay performs the login once
from the Keychain credential, stores the session, and attaches it to later operations.
The operations themselves stay ordinary HTTP requests.

The full manifest shape for every protocol and credential type is in
[`CAPABILITIES.md`](CAPABILITIES.md).
