# Relay — Detailed Build Plan

The local capability runtime for AI agents.

Relay is a macOS-first platform for turning external services and local capabilities into self-describing, AI-native tools.

A Relay tool is distributed as a portable executable containing:

1. A machine-readable capability manifest
2. An embedded SKILL.md describing how an AI should use the tool
3. A generic Relay runtime capable of executing the declared operations

A long-running Relay daemon provides shared infrastructure such as authentication, sessions, Keychain access, browser automation, HTTP transport, permissions, registration, telemetry, and MCP.

The key architectural idea is:

```
                    AI Agent / LLM
                          │
                    ┌─────┴─────┐
                    │   Relay   │
                    │    MCP    │
                    └─────┬─────┘
                          │
                    Relay Daemon
              ┌───────────┼───────────┐
              │           │           │
           Auth       Sessions     Browser
           Keychain   Permissions  HTTP
              │           │           │
              └───────────┼───────────┘
                          │
              ┌───────────┼───────────┐
              ▼           ▼           ▼
           github       slack       stripe
            tool         tool        tool
              │           │           │
          manifest     manifest    manifest
          SKILL.md     SKILL.md    SKILL.md
              │           │           │
             REST       GraphQL      gRPC
```

The fundamental invariant is:

Tool = executable + manifest + skill

The daemon is the runtime.

The manifest is machine truth.

The skill is LLM guidance.

MCP is an adapter.

The underlying protocol is an implementation detail.

⸻

1. Goals

Relay should make it extremely easy to turn a service into something that both humans and AI agents can use.

For example:

github.yaml
    ↓
relay build github.yaml
    ↓
github binary

The resulting binary should be able to:

github --describe
github --skill
github --help
github --version
github --json ...

It should be installable on another Mac without requiring the original YAML file.

relay install ./github

After installation:

AI agent
   ↓
Relay MCP
   ↓
Relay daemon
   ↓
github executable
   ↓
GitHub API

The same underlying capability should also be directly usable from the terminal:

github get-repository --owner openai --repo relay

⸻

2. Non-Goals for the MVP

Do not build all of the following initially:

* Cloud control plane
* Hosted tool registry
* Marketplace
* Complex agent orchestration
* Distributed daemon
* Remote execution
* Automatic skill generation using an LLM
* GraphQL and gRPC simultaneously with REST
* Complex GUI
* Multi-user permissions

The first version should prove the core loop:

Manifest → Build → Binary → Register → Execute → MCP

⸻

3. Core Components

Relay consists of four primary components.

3.1 relay

The developer/user CLI.

Responsibilities:

* Build tools
* Install tools
* Register tools
* Start/stop daemon
* Inspect registry
* Debug tools
* Manage configuration
* Expose MCP server

Examples:

relay build github.yaml --skill SKILL.md
relay install ./github
relay list
relay inspect github
relay daemon start
relay logs -n 50
relay mcp
relay auth status github
relay stats

⸻

3.2 Generated Tool Binary

Example:

github

The binary contains:

┌───────────────────────────┐
│ Relay generic runtime     │
├───────────────────────────┤
│ Embedded manifest         │
├───────────────────────────┤
│ Embedded SKILL.md         │
└───────────────────────────┘

It does not contain a custom implementation of every API endpoint.

Instead, the generic Relay runtime interprets the manifest and delegates execution to the Relay daemon.

This keeps generated binaries:

* Small
* Consistent
* Easy to upgrade
* Protocol-neutral
* Easy to reason about

3.3 Relay Daemon

The daemon is the central local runtime.

Example socket:

~/.relay/run/daemon.sock

Responsibilities:

* Tool registry
* Tool discovery
* Tool invocation
* Authentication
* macOS Keychain integration
* OAuth
* Browser/session management
* HTTP execution
* GraphQL execution
* gRPC execution
* Permission enforcement
* Telemetry
* MCP
* Runtime compatibility
* Tool lifecycle

The daemon should run continuously in the background.

On macOS it can eventually be installed as a LaunchAgent.

3.4 MCP Server

Relay exposes an MCP server so external LLM clients can discover and invoke Relay tools.

MCP should not become the core abstraction.

Instead:

                    ┌─────────────┐
                    │ Relay Core  │
                    └──────┬──────┘
                           │
                ┌──────────┴──────────┐
                │                     │
              CLI                    MCP
                │                     │
                └──────────┬──────────┘
                           │
                     Tool Registry
                           │
                       Execution

This ensures MCP and CLI have identical capabilities.

⸻

4. Repository Structure

Recommended Go repository:

relay/
├── cmd/
│   ├── relay/          # developer/user CLI
│   ├── relayd/         # the daemon
│   └── relay-tool/     # the generic runtime, without embedded assets
│
├── internal/
│   ├── manifest/
│   ├── runtime/
│   ├── build/
│   ├── skill/
│   ├── daemon/
│   ├── registry/
│   ├── ipc/
│   ├── auth/
│   ├── keychain/
│   ├── browser/
│   ├── protocol/
│   │   ├── rest/       # REST executor
│   │   ├── graphql/    # GraphQL executor
│   │   ├── local/      # LocalExecutor (filesystem, git)
│   │   └── grpc/
│   ├── mcp/
│   ├── telemetry/
│   ├── permissions/
│   ├── paths/
│   └── fsutil/
│
├── pkg/
│   ├── relay/          # public wire types + error model
│   └── toolruntime/    # the runtime embedded into every built tool
│
├── templates/
│
├── examples/
│   ├── github/         # github.yaml + SKILL.md (canonical pair)
│   ├── slack/          # slack.yaml + SKILL.md
│   ├── filesystem/     # filesystem.yaml + SKILL.md (local capability)
│   └── stripe/         # stripe.yaml (manifest-only draft)
│
├── skills/
│
├── docs/
│   ├── DESIGN.md
│   └── CONTRIBUTING.md
│
├── scripts/e2e.sh
├── Makefile
├── go.mod
├── go.sum
├── TASKS.md
└── README.md

⸻

5. Why Go

Go is a good fit for Relay because the primary product is a collection of portable executables.

Advantages:

* Excellent single-binary distribution
* Easy cross compilation
* Strong HTTP support
* Good concurrency
* Unix socket support
* macOS integration through system APIs/commands
* Mature CLI ecosystem
* Straightforward embedding
* Small operational footprint

The architecture should avoid coupling Relay's public model to Go.

The manifest and CLI contracts are more important than the implementation language.

⸻

6. Relay Manifest

The manifest defines the machine-readable capabilities of a tool.

Example:

apiVersion: relay/v1
kind: Tool
metadata:
  name: github
  version: 1.0.0
  description: GitHub repository and pull request operations
protocol:
  type: rest
  baseUrl: https://api.github.com
auth:
  type: oauth2
  provider: github
tools:
  - name: get_repository
    description: Get information about a GitHub repository
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
  - name: list_pull_requests
    description: List pull requests for a repository
    input:
      type: object
      properties:
        owner:
          type: string
        repo:
          type: string
        state:
          type: string
    request:
      method: GET
      path: /repos/{owner}/{repo}/pulls

The manifest should describe what the tool can do, not teach an LLM how to accomplish a workflow.

That distinction is important.

⸻

7. Manifest vs Skill

Relay intentionally separates machine-readable capability from LLM guidance.

Manifest

The manifest answers:

What operations exist and what inputs do they require?

Skill

SKILL.md answers:

How should an AI agent use these operations effectively?

For example:

# GitHub Skill

Use this tool to interact with GitHub repositories and pull requests.

## Repository Investigation

When investigating a repository:

1. Call `get_repository`.
2. If the user asks about recent changes, call `list_pull_requests`.
3. Read only what the task asks about rather than retrieving more than it needs.

## Pull Request Investigation

A useful sequence is:

get_repository
→ list_pull_requests

The skill is guidance.

The manifest is schema.

Do not duplicate the schema inside SKILL.md.

⸻

8. Embedded Skills

Every generated Relay tool should embed its own skill.

For example:

github
├── embedded manifest
└── embedded SKILL.md

The skill should be accessible through:

github --skill

Output:

# GitHub Skill
...

The daemon should also be able to retrieve it.

This creates a strong property:

A Relay binary carries its own documentation for AI usage.

There is no dependency on a remote documentation server.

⸻

9. Binary Contract

Every Relay-generated binary must implement a stable interface.

--describe

Returns JSON.

Example:

{
  "apiVersion": "relay/v1",
  "kind": "Tool",
  "name": "github",
  "version": "1.0.0",
  "description": "GitHub repository and pull request operations",
  "protocol": "rest",
  "runtime": {
    "name": "relay",
    "apiVersion": "v1"
  },
  "skill": true,
  "capabilities": [
    "network",
    "keychain"
  ],
  "tools": [
    {
      "name": "get_repository",
      "description": "Get information about a GitHub repository"
    },
    {
      "name": "list_pull_requests",
      "description": "List pull requests for a repository"
    }
  ]
}

This is the primary registration/discovery contract.

⸻

--skill

Returns the embedded SKILL.md.

⸻

--help

Human-oriented CLI help.

⸻

--version

Returns:

github 1.0.0

⸻

10. CLI Design

Human usage:

github get-repository \
  --owner openai \
  --repo relay

Machine usage:

github --json get-repository \
  --owner openai \
  --repo relay

The output contract should be:

stdout = machine result
stderr = logs/human diagnostics
exit code = success/failure

Never mix logs into JSON stdout.

⸻

11. CLI Argument Model

A tool definition:

tools:
  - name: get_repository

maps naturally to:

github get-repository

Arguments are generated from the input schema.

Example:

input:
  properties:
    owner:
      type: string
    repo:
      type: string

becomes:

github get-repository \
  --owner openai \
  --repo relay

The CLI should also support JSON input for complex objects:

github get-repository --input request.json

github get-repository --input-json '{"owner":"openai","repo":"relay"}'

Both are implemented; an explicitly passed flag wins over either form.

⸻

12. Build Process

The developer starts with:

github.yaml
github/SKILL.md

Build:

relay build github.yaml --skill SKILL.md [--out PATH]

Relay should:

1. Parse YAML
2. Validate manifest
3. Validate skill
4. Package manifest
5. Package skill
6. Produce generic Relay executable
7. Embed metadata
8. Produce final binary

Output:

dist/github

The generated binary should be self-contained. When `--out` is omitted the
binary also gets a PATH shim in `~/.relay/bin`; `--source DIR` points the build
at the Relay source tree explicitly, and `--keep` leaves the generated build
directory in place for inspection.

⸻

13. Generic Tool Runtime

Do not generate endpoint-specific Go code unless there is a compelling reason.

Instead:

Relay Tool Runtime
       │
       ├── Load embedded manifest
       ├── Load embedded skill
       ├── Parse CLI invocation
       ├── Validate input
       ├── Connect to daemon
       └── Send invocation request

The daemon performs the actual capability execution.

This keeps every generated tool behaviorally consistent.

⸻

14. Daemon IPC

Use a Unix domain socket:

~/.relay/run/daemon.sock

Communication can initially use JSON over the Unix socket.

Example request:

{
  "type": "invoke",
  "tool": "github",
  "operation": "get_repository",
  "input": {
    "owner": "openai",
    "repo": "relay"
  }
}

Response:

{
  "success": true,
  "result": {
    "name": "relay"
  }
}

Unix sockets provide a simple local trust boundary without opening a TCP port.

⸻

15. Tool Registration

Install:

relay install ./github

Relay should execute:

./github --describe

Then:

1. Parse descriptor
2. Validate descriptor
3. Check compatibility
4. Register tool
5. Store metadata
6. Make it available through CLI/MCP

Registry example:

~/.relay/
├── bin/
│   └── github -> ../tools/github
├── cache/
├── config/
├── logs/
│   └── daemon.log
├── registry/
│   ├── github.json
│   ├── slack.json
│   └── stripe.json
├── run/
│   ├── daemon.sock
│   └── daemon.lock
├── telemetry/
│   └── usage.jsonl
└── tools/
    └── github

⸻

16. Registration Metadata

Store something like:

{
  "name": "github",
  "version": "1.0.0",
  "path": "/Users/me/.relay/tools/github",
  "installedAt": "...",
  "runtime": "relay/v1"
}

Do not treat the registry as the source of truth for tool schemas.

The binary remains authoritative.

The registry is discovery metadata.

⸻

17. Automatic Registration

Registration is explicit: `relay install ./github` is the only way a tool
enters the registry. A tool binary carries no `--register` flag and there is no
registration on first execution, so nothing a tool does can add itself to the
registry. Automatic registration would raise questions the current model has no
answer for and is deliberately out of scope.

⸻

18. Execution Pipeline

The full path should be:

User / Agent
     │
     ▼
github CLI
     │
     ▼
Relay IPC
     │
     ▼
Relay Daemon
     │
     ├── permission check
     ├── credential lookup
     ├── session lookup
     ├── protocol selection
     │
     ▼
Protocol Adapter
     │
     ▼
External Service

For example:

github get_repository
        ↓
daemon
        ↓
OAuth credential from Keychain
        ↓
REST adapter
        ↓
api.github.com

⸻

19. Protocol Abstraction

Do not make REST the fundamental Relay abstraction.

Define a protocol interface.

Conceptually:

type Executor interface {
    Execute(ctx context.Context, request Request) (Response, error)
}

Implement:

RESTExecutor
GraphQLExecutor
GRPCExecutor
BrowserExecutor
LocalExecutor

The manifest specifies the protocol.

Example:

protocol:
  type: rest

GraphQL is implemented today:

protocol:
  type: graphql

The local capability family is implemented too, so a tool can describe the
machine it runs on rather than a remote API (spec 46):

protocol:
  type: local

gRPC and browser are not:

protocol:
  type: grpc

The CLI and MCP layers should not care.

⸻

20. REST MVP

REST should be the first protocol.

Support:

* GET
* POST
* PUT
* PATCH
* DELETE
* Path parameters
* Query parameters
* Headers
* JSON bodies
* JSON responses
* Pagination — an optional `pagination:` block on `request:` declares how a
  collection operation walks its pages. Two strategies exist:
  * `link-header` — follow the RFC 8288 `Link rel="next"` response header
    (GitHub, Stripe, most REST APIs).
  * `cursor` — echo a value read from the JSON response body back into a named
    request parameter, with an optional has-more field.

  Example:

  request:
    method: GET
    path: /repos/{owner}/{repo}/pulls
    pagination:
      style: link-header

  Only GET and HEAD may declare one. Following pages is opt-in and turned on by
  the caller; a walk is bounded by a maximum page count and a maximum total
  response size, and a partial result is reported as truncated rather than
  passed off as a single page.
  The CLI turns a walk on with `relay run <tool> <operation> --paginate`; the
  default stays a single request, and a reply reports the page count and
  whether the walk was truncated.
* Authentication
* Error handling

Example:

request:
  method: GET
  path: /repos/{owner}/{repo}

Relay resolves:

{owner}
{repo}

from CLI/agent input.

⸻

21. Authentication

Authentication belongs to the daemon.

The generated binary should never need to directly manage secrets.

Architecture:

Tool
 ↓
Daemon
 ↓
Auth Manager
 ↓
macOS Keychain

Potential credential types (the validator accepts `api_key`, `bearer`,
`basic`, `oauth2`, and `client_credentials` today; `mTLS` is not supported):

API Key
Bearer Token
OAuth2
Basic Auth
Client Credentials
mTLS

The manifest describes authentication requirements.

Example:

auth:
  type: oauth2
  provider: github

The daemon handles the actual credential.

⸻

22. macOS Keychain

Relay should integrate with macOS Keychain.

Example conceptual namespace:

service: com.relay.github
account: default

Credentials should never appear in:

* CLI output
* MCP output
* logs
* telemetry
* manifest
* skill

The daemon should inject credentials at execution time.

⸻

23. Browser and Sessions

Some services cannot be cleanly represented as APIs.

Relay provides a shared session layer, rather than a browser runtime per tool: a
browser tool is an HTTP tool whose requests carry cookies the daemon obtained by
logging in.

Example:

Relay Browser
    │
    ├── Login            form POST, or a bearer/header login
    ├── Cookies          per-tool cookie jar, persisted under the Relay home
    ├── Sessions         explicit login and clear over IPC
    ├── OAuth            device flow lives in auth (spec 21); redirect login is not yet
    └── Web interaction  HTTP only; no JavaScript execution

This allows future tools to declare:

protocol:
  type: browser

without creating a separate browser runtime per tool.

The executor is built on net/http plus net/http/cookiejar. JavaScript-heavy
interaction — running page scripts, driving a DOM, following a rendered
navigation — is deliberately out of scope until a real browser engine lands. An
honest session layer that logs in and reuses cookies is more useful than a
stubbed engine, and it fills the same protocol.Executor seam a future engine
would attach to.

A browser tool declares its login flow additively under auth.login, keeping the
auth.type from the existing vocabulary:

protocol:
  type: browser
  baseUrl: https://api.example.com
auth:
  type: basic            # basic -> user:password; bearer/api_key -> token
  login:
    kind: form           # form | header
    path: /session
    usernameField: user
    passwordField: pass

The CLI surface is explicit: `relay session login <tool>` runs the declared
login and stores the session, and `relay session clear <tool>` forgets it.
Neither sends, returns, or prints a cookie, and both travel as IPC frames.
Operations carry the same request shape REST uses (method, path, query, headers,
body), so only the transport differs and neither the CLI nor MCP changes
(spec 19). The daemon owns the session: cookies are written under the Relay home
and never cross IPC, appear in a log, or reach telemetry. An operation with no
session fails AUTH_REQUIRED, and the client refuses to follow a redirect to a
different host so a cookie is never replayed to another origin (spec 22, 40).

⸻

24. Capability Model

Tools should explicitly declare capabilities.

Example:

capabilities:
  - network
  - keychain

Potential future capabilities:

network
keychain
browser
filesystem.read
filesystem.write
shell
notifications
clipboard

The daemon becomes the security boundary.

A tool asking for a new capability should not silently receive it.

⸻

25. Permissions

Potential permission model:

permissions:
  network:
    hosts:
      - api.github.com
  filesystem:
    read:
      - ~/Documents

For destructive operations:

delete_repository
delete_file
send_message
charge_customer

Relay should eventually support confirmation policies.

Example:

Tool wants to execute:
github.delete_repository
Allow? [y/N]

MCP clients should receive equivalent policy enforcement.

⸻

26. Error Model

Define structured errors.

Example:

{
  "success": false,
  "error": {
    "code": "AUTH_REQUIRED",
    "message": "GitHub authentication is required",
    "retryable": false
  }
}

Potential error codes:

INVALID_INPUT
TOOL_NOT_FOUND
OPERATION_NOT_FOUND
AUTH_REQUIRED
AUTH_FAILED
PERMISSION_DENIED
NETWORK_ERROR
TIMEOUT
RATE_LIMITED
REMOTE_ERROR
PROTOCOL_ERROR
RUNTIME_INCOMPATIBLE

AI agents benefit greatly from predictable errors.

⸻

27. MCP Adapter

Relay should expose all registered capabilities through MCP.

MCP discovery:

MCP Client
   ↓
Relay MCP Server
   ↓
Relay Registry
   ↓
Tool Descriptors

Invocation:

MCP tool call
      ↓
Relay capability invocation
      ↓
Relay daemon
      ↓
Tool binary
      ↓
Protocol adapter

MCP should not implement a separate execution engine.

Pagination crosses the same seam. When an operation's manifest declares a
`pagination:` strategy (spec 20), MCP advertises one extra, optional boolean
argument, `paginate`, alongside the operation's own properties; it is never
required and it is absent from every operation that cannot paginate. Setting it
forwards `InvokeRequest.Paginate` to the daemon exactly as
`relay run --paginate` does, so MCP gains the walk without an execution engine
of its own. The walk's shape (— the page count and whether the walk was
truncated —) is reported inside the tool result, never as a stray stdout line,
so a bounded walk is never mistaken for a complete collection.

⸻

28. MCP Tool Naming

Possible convention:

github_get_repository
github_list_pull_requests

This makes tools easy to discover.

Alternatively, expose a namespace:

github.get_repository
github.list_pull_requests

The namespace approach is preferable if supported cleanly by the client ecosystem.

⸻

29. MCP Skill Exposure

Relay should expose the tool's skill to MCP-aware consumers.

Conceptually:

github
 ├── tools
 ├── manifest
 └── skill

The important property is that MCP sees the same tool definition and skill as the CLI.

There should never be:

CLI definition ≠ MCP definition

Instead:

                 Manifest
                    │
              Capability Model
                 /       \
               CLI       MCP

Pagination is one projection of that shared model, not a place where the two
definitions diverge. The manifest still declares the only input schema; MCP
derives its advertised schema from it and adds the reserved `paginate` argument
solely on operations that declare a `pagination:` strategy. There is no
MCP-only schema and no MCP-only pagination surface: `paginate` is the adapter's
name for the same walk opt-in the CLI spells `--paginate`, and both reach the
daemon as `InvokeRequest.Paginate`.

⸻

30. Skill Design

Skills should focus on:

* Usage patterns
* Workflow sequences
* Important constraints
* Choosing between operations
* Common mistakes
* Pagination strategies
* Recommended operation ordering
* Interpretation of results

They should not become a second schema.

Bad:

get_repository accepts:
owner: string
repo: string

The manifest already contains this.

Better:

When investigating a repository, first retrieve repository metadata
before querying pull requests.

For a repository investigation, use:

get_repository
→ list_pull_requests

⸻

31. Usage Telemetry

One of the more interesting properties of Relay is that it can observe how capabilities are actually used.

Example:

get_repository
      ↓
list_pull_requests

Relay can collect local usage statistics such as:

Operation frequency
Operation sequences
Failure frequency
Input validation failures
Latency
Rate limits
Common workflows

This can eventually be used to improve skills.

For example, if agents repeatedly use:

get_repository
→ list_pull_requests

the GitHub skill can explicitly recommend that workflow.

⸻

32. Privacy-First Telemetry

Telemetry should initially be local.

Store aggregated usage:

~/.relay/telemetry/

Avoid collecting:

* OAuth tokens
* API keys
* Sensitive request bodies
* Personal data
* Full responses

Possible local event:

{
  "tool": "github",
  "operation": "get_repository",
  "timestamp": "...",
  "durationMs": 182
}

Later, users could explicitly opt into anonymized telemetry.

⸻

33. Skill Optimization

Longer term, Relay could use usage telemetry to identify:

Common operation sequences

and suggest skill improvements.

For example:

Observed workflow:
get_repository
→ list_pull_requests

Suggestion:
Add "Pull Request Investigation" workflow
to github/SKILL.md.

Eventually an LLM could generate proposed skill changes, but those changes should be reviewed rather than silently modifying installed tools.

⸻

34. Binary Portability

A major product goal is:

Build once, distribute the binary.

The recipient should not need:

* Go
* Python
* Node
* YAML
* API definitions
* Project source code

They should receive:

github

and install it:

relay install ./github

The only external dependency should be the Relay daemon/runtime compatibility.

⸻

35. Runtime Versioning

Embedded tools should declare:

{
  "runtime": {
    "name": "relay",
    "apiVersion": "v1"
  }
}

This allows Relay to reject incompatible tools cleanly.

Example:

Tool requires Relay runtime v2.
Installed daemon supports v1.
Please upgrade Relay.

Do not silently execute incompatible manifests.

⸻

36. Tool Versioning

Every tool should have:

name
version
manifest version
skill version
runtime version

Example:

github
1.4.0
relay/v1

Manifest and skill should be versioned together.

A skill describing behavior for version 1.4.0 should not accidentally be paired with the manifest for 2.0.0.

⸻

37. Local Installation

A simple installation flow. There is no packaged installer yet (no Homebrew
formula and no release artifact), so start by building the CLI and daemon from
the source tree:

export PATH="$PATH:/usr/local/go/bin"
make build                       # -> dist/relay, dist/relayd
export PATH="$PWD/dist:$PATH"

Then:

relay daemon install
relay daemon start

Install a tool:

relay install ./github

Inspect:

relay inspect github

List:

relay list

Run:

github get-repository --owner openai --repo relay

⸻

38. PATH Management

Relay-installed tools can live under:

~/.relay/tools/

For example:

~/.relay/tools/github
~/.relay/tools/slack
~/.relay/tools/stripe

Relay never edits your shell configuration. Add:

~/.relay/bin

to your PATH yourself.

Then:

github
slack
stripe

work naturally.

⸻

39. LaunchAgent

The daemon should eventually run automatically on macOS.

Conceptually:

~/Library/LaunchAgents/com.relay.daemon.plist

The user should not need to manually start Relay every time.

Expected state:

Mac starts
   ↓
Relay daemon starts
   ↓
Registry loaded
   ↓
MCP endpoint available
   ↓
Tools ready

⸻

40. Security Boundary

The Relay daemon is the trusted component.

Generated tool binaries should be treated as potentially untrusted.

Therefore:

Generated Tool
     ↓
Daemon
     ↓
Permission checks
     ↓
Credential injection
     ↓
Execution

Do not let arbitrary tool binaries directly access all credentials.

⸻

41. MCP Security

MCP should inherit the same permission system.

If:

CLI → GitHub → OAuth

requires GitHub credentials, then:

MCP → GitHub

must use exactly the same auth mechanism.

Avoid creating an MCP-only credential path.

That would create two security models and eventually two bugs.

⸻

42. Developer Experience

The ideal developer workflow should be extremely small.

Create:

github.yaml
SKILL.md

Build:

relay build github.yaml --skill ./SKILL.md

Result:

github

Inspect:

./dist/github --describe
./dist/github --skill

Install:

relay install ./dist/github

Use:

github get-repository --owner openai --repo relay

Connect an AI:

AI
 ↓
Relay MCP
 ↓
github

That should be the "aha" moment.

⸻

43. Example Complete Tool

Directory:

github/
├── github.yaml
└── SKILL.md

Manifest:

apiVersion: relay/v1
kind: Tool
metadata:
  name: github
  version: 1.0.0
  description: GitHub repository and pull request operations

runtime:
  name: relay
  apiVersion: v1

protocol:
  type: rest
  baseUrl: https://api.github.com

auth:
  type: oauth2
  provider: github

capabilities:
  - network
  - keychain

permissions:
  network:
    hosts:
      - api.github.com

tools:
  - name: get_repository
    description: Get information about a GitHub repository
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

  - name: list_pull_requests
    description: List pull requests for a repository
    input:
      type: object
      properties:
        owner:
          type: string
        repo:
          type: string
        state:
          type: string
          enum:
            - open
            - closed
            - all
          default: open
      required:
        - owner
        - repo
    request:
      method: GET
      path: /repos/{owner}/{repo}/pulls

The skill is the guidance half and lives beside the manifest in
`examples/github/SKILL.md`: when to use the tool, the recommended workflow, the
pagination limit, and how to read the result. It is guidance, not a schema.

Build:

relay build github.yaml --skill SKILL.md

Result:

github

⸻

44. GraphQL

GraphQL uses the same architecture as REST and has an executor in the daemon.

Manifest:

protocol:
  type: graphql
  endpoint: https://api.example.com/graphql

Tool:

request:
  document: |
    query GetUser($id: ID!) {
      user(id: $id) {
        id
        name
      }
    }

The generated CLI does not change.

The MCP representation does not change.

Only the protocol executor changes.

⸻

45. gRPC

Implemented by GRPCExecutor behind the same protocol.Executor seam as REST and
GraphQL. The server address comes from the protocol block, and each operation
names its method literally in the request block:

protocol:
  type: grpc
  endpoint: localhost:50051

tools:
  - name: get_user
    input:
      type: object
      properties:
        id:
          type: string
      required:
        - id
    request:
      package: example.user
      service: UserService
      method: GetUser

request.package and request.service name the service the way the protocol block
names a service, and request.method names the RPC method; together they address
/package.Service/Method. The three are meaningful only for protocol.type grpc,
must be declared together, and must not be combined with request.path. An
operation may instead keep addressing its method by the canonical path in
request.path, so a manifest written before these fields existed is unchanged;
the validator rejects declaring both forms at once as ambiguous.

The executor resolves the request and response message descriptors through
server reflection at call time, so a tool needs no generated Go stubs. It sends
the declared request.body (or the operation input when no body is declared) as
protojson and returns the response as JSON, so the wire shape the CLI and MCP
surfaces see is the same JSON shape REST and GraphQL produce. Manifest headers
become gRPC metadata, the daemon-injected credential is attached the way it is
for REST, and the call's deadline comes from the context. Failures map onto the
shared taxonomy (§26): an unreachable server is NETWORK_ERROR, an expired
deadline is TIMEOUT, any other non-OK status is REMOTE_ERROR naming the status
code, and a malformed request block or unresolvable method is PROTOCOL_ERROR (a
missing address is INVALID_INPUT).

The daemon routes execution to:

GRPCExecutor

The capability abstraction remains identical.

⸻

46. Local Capabilities

Relay supports tools that do not represent remote APIs. A local tool has no
service to contact: its manifest declares `protocol.type: local`, and each
operation's request block names the local primitive it runs. The built-in
LocalExecutor implements two families today:

filesystem
 ├── read_file
 ├── write_file
 ├── list_directory
 └── stat

git
 ├── status
 ├── diff
 └── log

The filesystem operations call the Go standard library in-process. The git
operations invoke the `git` binary as a read-only inspection of a working tree;
the binary is executed directly with an argument vector, never through a shell,
and optional index locks and external diff/textconv helpers are disabled so a
read cannot mutate the repository.

Manifest shape. A local request names an operation instead of a transport, so it
carries `operation` and none of the REST fields (method, path) or the GraphQL
fields (document, variables). The validator rejects the other protocols' fields
on a local request rather than ignoring them, and rejects `protocol.baseUrl`.

Example:

apiVersion: relay/v1
kind: Tool
metadata:
  name: filesystem
  version: 0.2.0
  description: Local filesystem capabilities — read, write, list, and stat

protocol:
  type: local

capabilities:
  - filesystem.read
  - filesystem.write

permissions:
  filesystem:
    read:
      - "~/Documents"
      - "~/Desktop"
    write:
      - "~/Documents"

tools:
  - name: read_file
    description: Read the contents of a single file
    input:
      type: object
      properties:
        path:
          type: string
      required:
        - path
    request:
      operation: read_file

The operation's name is carried in the protocol-neutral `Spec.Endpoint` slot, so
the daemon, and therefore the CLI and MCP surfaces, reach a local operation
through the identical path they use for REST and GraphQL. The generated CLI does
not change and the MCP representation does not change; only the executor does.

Security. The daemon remains the security boundary (spec 24, 40). Before a local
operation runs, the daemon derives the concrete path from the operation input,
resolves it — expanding a leading tilde and following symbolic links — and
checks it against the declared `permissions.filesystem` scopes. Because a
symlink is resolved before the check, a link that sits inside a declared scope
but points outside it is refused: the check sees the real target, not the link.
The executor resolves the target by the same rule, so the path that was
authorized and the path that is acted on are identical. A local tool that
declares no filesystem capability is refused outright, the correct default-deny
reading. Output is capped at 8 MiB, matching the IPC and MCP frame limits, so an
operation cannot hand the transport a result it could not carry.

This is why the product should not be called something API-specific. Relay is a
capability layer, not an API wrapper.

⸻

47. Future Tool Registry

Eventually Relay can have a registry.

Nothing in this section exists yet: there is no `relay search`, and `relay
install` takes a local binary path, not a tool name.

Example:

relay search github
relay install github

Registry entries:

github
slack
stripe
jira
linear
notion
aws

The registry could contain:

Tool binary
Manifest
Skill
Version
Signature
Publisher
Compatibility

However, this should come after the local binary workflow works.

⸻

48. Signed Tools

Future tools should be cryptographically signed.

Example:

relay install github

Relay verifies:

Publisher
Signature
Manifest
Binary

This becomes important once third-party tools exist.

⸻

49. Tool Trust Levels

Potential model:

Trusted
Verified
Unknown
Blocked

Example:

github
Publisher: GitHub
Status: Verified
Capabilities:
  network
  keychain

A third-party binary requesting:

filesystem.write
shell
keychain

should receive much more scrutiny.

⸻

50. Agent-Native Future

The long-term vision is larger than API wrappers.

Relay can become the local capability substrate for AI agents:

                     Agent
                       │
                 ┌─────┴─────┐
                 │   Relay   │
                 └─────┬─────┘
                       │
        ┌──────────────┼──────────────┐
        │              │              │
       Web           APIs           Local
        │              │              │
     Browser        REST/gRPC       Git
     Sessions       GraphQL         Docker
        │              │              │
        └──────────────┼──────────────┘
                       │
                  Permissions
                  Credentials
                  Sessions
                  Telemetry

Relay becomes the local operating layer through which an AI interacts with the user's digital environment.

⸻

51. MVP

The first release should contain only:

Required

Manifest

* YAML parser
* Validation
* Tool definitions
* JSON schemas
* REST requests

Tool Builder

relay build

Generated Binary

tool --describe
tool --skill
tool --help
tool --version
tool operation ...

Daemon

* Unix socket
* Registry
* Invocation
* REST execution

Authentication

* API key
* macOS Keychain

Skills

* Embedded SKILL.md
* --skill

MCP

* Tool discovery
* Tool invocation

CLI

relay build
relay install
relay list
relay inspect
relay daemon start|stop|restart|status|install
relay logs
relay mcp
relay auth login|logout|status
relay stats
relay version

⸻

52. Phase 1 — Manifest + Runtime

Implement:

Manifest
   ↓
Validator
   ↓
Generic Runtime
   ↓
Binary

Deliver:

relay build github.yaml

and:

github --describe
github --skill

No daemon initially if necessary.

The goal is proving the binary format.

⸻

53. Phase 2 — Daemon

Implement:

relayd

with:

* Unix socket
* registry
* invocation
* REST execution

Then change:

Tool → API

into:

Tool → Daemon → API

⸻

54. Phase 3 — Authentication

Add:

Keychain
API keys
Bearer tokens
OAuth foundation

Credentials remain daemon-owned.

⸻

55. Phase 4 — Skills

Add:

SKILL.md

embedding.

Implement:

tool --skill

and daemon/MCP skill retrieval.

⸻

56. Phase 5 — MCP

Implement:

relay mcp

Expose:

Registry
Tools
Schemas
Skills
Invocation
Errors

Reuse the exact same execution engine.

⸻

57. Phase 6 — Telemetry

Start with local telemetry.

Track:

tool
operation
latency
success/failure
sequence

Do not collect sensitive payloads.

⸻

58. Phase 7 — Additional Protocols

Add:

GraphQL
gRPC
Browser
Local

without changing the public capability model.

⸻

59. Testing Strategy

Tests should exist at several levels.

Manifest Tests

Validate:

* Required fields
* Schema correctness
* Duplicate operation names
* Invalid protocol
* Invalid auth

Runtime Tests

Test:

CLI parsing
Input validation
JSON output
Exit codes

Daemon Tests

Test:

Registration
Invocation
IPC
Registry
Errors

Protocol Tests

Test:

REST
GraphQL
gRPC

against mocked services.

Security Tests

Test:

Unauthorized tool
Missing credentials
Permission violations
Credential leakage
MCP permission bypass

⸻

60. End-to-End Test

A complete test should look like:

relay build examples/github/github.yaml --skill examples/github/SKILL.md

Then:

./dist/github --describe

Verify manifest.

Then:

relay install ./dist/github

Verify registry.

Then:

github get-repository \
  --owner openai \
  --repo relay

Verify API result.

Then:

github --skill

Verify skill.

Then connect an MCP client.

Verify:

MCP discovery
      ↓
github_get_repository
      ↓
Relay daemon
      ↓
REST
      ↓
GitHub

The same capability must produce the same result through both CLI and MCP.

⸻

61. Definition of Done

Relay MVP is complete when a developer can:

1. Define a service

github.yaml

2. Define its AI guidance

SKILL.md

3. Build one binary

relay build github.yaml --skill SKILL.md

4. Distribute it

github

5. Install it

relay install ./github

6. Inspect it

github --describe
github --skill

7. Execute it

github get-repository ...

8. Expose it to an LLM

Relay MCP

9. Execute the same capability through MCP

LLM → MCP → Relay → github → API

10. Observe usage

Relay locally records capability usage patterns.

⸻

62. Final Architecture

                          ┌───────────────────┐
                          │     AI Agent      │
                          └─────────┬─────────┘
                                    │
                                   MCP
                                    │
                          ┌─────────▼─────────┐
                          │      Relay        │
                          │    MCP Server     │
                          └─────────┬─────────┘
                                    │
                                    │
              ┌─────────────────────▼────────────────────┐
              │             Relay Daemon                 │
              │                                          │
              │  Registry                                │
              │  Permissions                             │
              │  Authentication                          │
              │  Keychain                                │
              │  Sessions                                │
              │  Browser                                 │
              │  Telemetry                               │
              │  Protocol Routing                        │
              └─────────────────────┬────────────────────┘
                                    │
                         Unix Socket IPC
                                    │
             ┌──────────────────────┼──────────────────────┐
             │                      │                      │
             ▼                      ▼                      ▼
        ┌─────────┐            ┌─────────┐            ┌─────────┐
        │ github  │            │  slack  │            │ stripe  │
        │ binary  │            │ binary  │            │ binary  │
        ├─────────┤            ├─────────┤            ├─────────┤
        │Manifest │            │Manifest │            │Manifest │
        │SKILL.md │            │SKILL.md │            │SKILL.md │
        └────┬────┘            └────┬────┘            └────┬────┘
             │                      │                      │
             ▼                      ▼                      ▼
           REST                  GraphQL                 REST
             │                      │                      │
             ▼                      ▼                      ▼
          GitHub                  Slack                  Stripe

⸻

63. The Core Product Idea

The most important thing to preserve while implementing Relay is this:

                ┌─────────────────────┐
                │       Relay         │
                │                     │
                │  Capability Runtime │
                └──────────┬──────────┘
                           │
             ┌─────────────┴─────────────┐
             │                           │
       Machine Layer               Intelligence Layer
             │                           │
         Manifest                     Skill
             │                           │
       "What can I do?"          "How should I use it?"
             │                           │
             └─────────────┬─────────────┘
                           │
                       One Binary
                           │
                    One Distribution
                           │
                CLI + MCP + future agents

Relay is not an API wrapper generator.

It is a local capability runtime where external services, local software, and eventually arbitrary capabilities can be packaged as self-describing, AI-native tools.

The executable carries both the ability and the knowledge of how to use that ability, while the daemon provides the shared infrastructure required to safely execute it.
