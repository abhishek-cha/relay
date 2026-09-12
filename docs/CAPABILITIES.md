# Capabilities and configuration

This is the complete YAML surface a Relay manifest can declare, and what the daemon
does with it. It is the reference for authoring a tool; the walkthrough in the
[README](../README.md#package-your-own-api) is the place to start if you have not
built one yet.

Two layers are at work, and keeping them separate is the whole design:

- the **manifest** declares what the tool needs — operations, inputs, credentials,
  capabilities;
- the **daemon** enforces it — it owns the credentials, checks the declared
  capabilities, and refuses anything a manifest did not ask for.

A built tool binary is treated as untrusted. It cannot reach the Keychain, cannot
widen its own permissions, and cannot call a service the daemon has not authorised.

---

## The capability model

`capabilities` and `permissions` state what a tool is allowed to touch. The model is
**opt-in per tool**, which matters more than it sounds:

| Manifest declares | Behaviour |
| --- | --- |
| neither `capabilities` nor `permissions` | runs unchecked — the tool is treated as pre-model, so tools built before this existed keep working |
| either one | held **default-deny** — an unnamed capability is refused, and a host or path outside the declared scope is refused |

The moment you declare one key you opt into the whole model, so declare everything the
tool needs. Partial declarations fail closed, which is the intended surprise.

Outside that opt-out sits the **destructive-operation gate**. It is driven by daemon
configuration rather than by the manifest, so an operation on the gated list requires
explicit confirmation whether or not the tool declares a capability surface. Without a
confirmation the call returns `PERMISSION_DENIED`; `relay run` prompts on an interactive
terminal, and MCP carries no token, so an agent's call is denied.

---

## Capability vocabulary

`capabilities` draws from a fixed set. An unknown name fails the build.

| Capability | Grants | Narrowed by |
| --- | --- | --- |
| `network` | outbound HTTP | `permissions.network.hosts` |
| `keychain` | read its own stored credential | — |
| `browser` | a stored login session | — |
| `filesystem.read` | read files | `permissions.filesystem.read` |
| `filesystem.write` | write files | `permissions.filesystem.write` |
| `shell` | run a local process | — |
| `notifications` | post a macOS notification | — |
| `clipboard` | read or write the clipboard | — |

`filesystem.*`, `shell`, `notifications`, and `clipboard` describe local capabilities,
which are a reserved part of the vocabulary rather than a supported authoring path.

---

## Declaring capabilities and permissions

```yaml
capabilities:
  - network
  - keychain

permissions:
  network:
    hosts:
      - api.example.com
      - uploads.example.com
```

```yaml
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
```

A host entry matches the host the request actually goes to, so list every host the tool
can reach, including any a redirect or a paginated walk might land on. Filesystem scopes
are resolved before use: `~` is expanded and symlinks are followed, and the call is
refused unless a declared scope contains the resolved path.

A capability you do not declare is one you do not have. There is no way for a tool to
grant itself a capability at runtime, and adding one to a manifest is a visible change to
the file a user reads before installing it.

---

## Credential setups

`auth` describes the credential the daemon must hold. The secret itself never appears in
the manifest and never reaches the tool binary — it lives in the macOS Keychain under
`com.relay.<tool>`.

| `auth.type` | Sent on the wire as |
| --- | --- |
| `api_key` | `X-API-Key: <secret>` |
| `bearer` | `Authorization: Bearer <secret>` |
| `basic` | HTTP Basic |
| `client_credentials` | HTTP Basic |
| `oauth2` | `Authorization: Bearer <token>` |

```yaml
auth:
  type: api_key
```

```yaml
auth:
  type: bearer
  provider: slack
```

`provider` is a label only; it does not select any behaviour. Omit `auth` entirely for a
tool that needs no credential.

### OAuth2

Three login flavours share one declaration. `relay auth login <tool>` picks whichever the
manifest describes, trying the browser flow first, then the device grant, then the paste
prompt — so the command you type does not change.

**Paste a token.** An `oauth2` block with no endpoints means a human supplies the token:

```yaml
auth:
  type: oauth2
```

**Device authorization grant** (RFC 8628). The CLI prints a code and waits while the
daemon polls:

```yaml
auth:
  type: oauth2
  deviceAuthorizationEndpoint: https://example.com/oauth/device/code
  tokenEndpoint: https://example.com/oauth/token
  clientId: relay-tool
  scopes:
    - read
    - write
```

**Browser authorization code with PKCE.** Relay binds a loopback listener, hands the CLI
an authorize URL to open, and completes the exchange when the redirect arrives:

```yaml
auth:
  type: oauth2
  authorizationEndpoint: https://example.com/oauth/authorize
  tokenEndpoint: https://example.com/oauth/token
  clientId: relay-tool
  scopes:
    - read
  redirectURI: http://127.0.0.1:8765/callback   # optional
```

Rules the build enforces, because each one is a way to lose a code to someone else:

- the browser and device flows are mutually exclusive — declare the endpoints of one;
- `tokenEndpoint` and `clientId` are required whenever `authorizationEndpoint` is set;
- `redirectURI` must be `http` on `127.0.0.1` or `localhost`, with no query and no
  fragment. Omit it and Relay binds an ephemeral loopback port instead;
- there is no `clientSecret` field. Both flows are public clients, deliberately.

### Browser login

A `browser` tool turns a credential into a **session** rather than a header. The `login`
block sits under `auth` and is additive, exactly like the OAuth2 endpoint fields:

```yaml
auth:
  type: basic
  login:
    kind: form
    method: POST
    path: /login
    usernameField: email
    passwordField: password
```

| Field | Notes |
| --- | --- |
| `kind` | `form` posts the credential as form fields; `header` sends it in a request header |
| `path` | required; the login endpoint, relative to `protocol.baseUrl` |
| `method` | defaults to `POST` |
| `usernameField` / `passwordField` | required for `form` |
| `header` | required for `header`; the header the credential is sent in |
| `scheme` | optional; a prefix such as `Bearer` |

A header login instead of a form:

```yaml
auth:
  type: bearer
  login:
    kind: header
    path: /api/session
    header: Authorization
    scheme: Bearer
```

The credential comes from the Keychain, and the resulting cookie is held by the daemon:

```sh
relay auth login dashboard       # stores the credential
relay session login dashboard    # runs the login flow, stores the session
relay session clear dashboard    # forgets it
```

A browser tool with no `login` block needs no session; asking to log one in fails loudly
rather than guessing at a flow.

---

## Protocol setups

`protocol` selects the executor. The seam is one interface, so the CLI, the MCP adapter,
and the operation shapes above are identical whichever you choose.

### REST

```yaml
protocol:
  type: rest
  baseUrl: https://api.example.com

tools:
  - name: update_thing
    description: Update one thing
    input:
      type: object
      properties:
        id:
          type: string
        body_payload:
          type: object
          description: Fields to change
        trace:
          type: string
          description: A value to send as a header
      required:
        - id
    request:
      method: PATCH
      path: /things/{id}
      headers:
        X-Trace: "{trace}"
      body:
        payload: "{body_payload}"
```

| Request key | Notes |
| --- | --- |
| `method` | `GET`, `POST`, `PUT`, `PATCH`, or `DELETE` |
| `path` | required; `{param}` placeholders are filled from input |
| `query` | map of parameter name to template, e.g. `limit: "{limit}"` |
| `headers` | map of header name to template |
| `body` | an explicit body; omit it and the remaining inputs are sent as JSON |
| `pagination` | optional; see [Pagination](#pagination) |

Two rules worth internalising:

- every `{param}` in `path` must name a declared input property **and** appear in
  `input.required` — the build refuses otherwise, because a placeholder you cannot fill is
  a broken operation;
- on a method that carries a body, **every input not consumed by a path, query, or header
  template is sent as a JSON body**. That is why most POST operations need no `body` block
  at all.

Path placeholders are `url.PathEscape`d; query and header values are substituted raw and
then encoded by `url.Values`, so do not escape them yourself.

### GraphQL

```yaml
protocol:
  type: graphql
  endpoint: https://api.example.com/graphql

tools:
  - name: get_user
    description: Fetch one user
    input:
      type: object
      properties:
        id:
          type: string
          description: The user id
      required:
        - id
    request:
      document: |
        query GetUser($id: ID!) {
          user(id: $id) { id name }
        }
      variables:
        id: id
```

`endpoint` is required. `variables` maps a GraphQL variable name to the input property
that supplies it; a variable whose name matches an input property needs no entry, which is
why `id: id` above could be omitted.

### gRPC

```yaml
protocol:
  type: grpc
  baseUrl: https://grpc.example.com

tools:
  - name: get_user
    description: Fetch one user
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
```

An operation addresses its method one of two ways, never both: the literal triple above,
which assembles `/example.user.UserService/GetUser`, or `request.path`. Declaring both is
ambiguous and rejected. The fields are gRPC-only; another protocol carrying them fails the
build.

### Browser

```yaml
protocol:
  type: browser
  baseUrl: https://dashboard.example.com

auth:
  type: basic
  login:
    kind: form
    path: /login
    usernameField: email
    passwordField: password

tools:
  - name: list_reports
    description: List the reports the logged-in account can see
    input:
      type: object
    request:
      method: GET
      path: /reports
```

Operations are ordinary HTTP requests that carry the stored session; there is no
JavaScript engine. Use this when a service is only reachable behind a login form. See
[`examples/browser/`](../examples/browser/browser.yaml) for a complete pair.

---

## Pagination

A REST operation can declare how a collection walks its pages. Declaring it never changes
the default call — the walk stays opt-in, so a plain invocation still makes exactly one
request.

**Link header** (RFC 8288 `Link rel="next"`), which covers GitHub, Stripe, and most REST
APIs:

```yaml
request:
  method: GET
  path: /things
  pagination:
    style: link-header
```

**Cursor**, where the next page's cursor comes from the response body and goes back as a
request parameter:

```yaml
request:
  method: GET
  path: /things
  pagination:
    style: cursor
    cursorParam: cursor
    cursorField: next_cursor
    cursorIn: query
```

| Key | Notes |
| --- | --- |
| `style` | `link-header` or `cursor` |
| `cursorParam` | cursor style only, required; the request parameter that carries the cursor |
| `cursorField` | cursor style only, required; dotted path in the JSON body holding the next cursor |
| `cursorIn` | cursor style only; `query` or `body` |
| `hasMoreField` | cursor style only; optional dotted path to a bool or number that stops the walk early |
| `limitParam` / `limit` | optional page-size parameter and its default, either style |

Fields are scoped to their style: a `link-header` operation that also declares
`cursorParam` is rejected, because it would never read it. Pagination is a `GET` or `HEAD`
strategy only.

Ask for a walk in whichever way you drive the tool:

```sh
mytool list-things --paginate             # the built binary
relay run mytool list_things --paginate   # through the relay CLI
```

Over MCP the same operation accepts a `paginate: true` argument. The walk's shape is
reported on stderr so stdout stays the pure result, and a walk that hit a bound is named
as truncated rather than silently returned as complete.

---

## Input schemas

Every operation needs `input` with `type: object`. Each property becomes a `--flag` on
the generated CLI, and a matching argument in MCP, so `owner` is `--owner`.

```yaml
input:
  type: object
  properties:
    owner:
      type: string
      description: Repository owner
    state:
      type: string
      description: Filter by state
      enum:
        - open
        - closed
      default: open
    labels:
      type: array
      description: Label names to filter by
      items:
        type: string
    since:
      type: string
      format: date-time
  required:
    - owner
```

| Key | Notes |
| --- | --- |
| `type` | `string`, `integer`, `number`, `boolean`, `array`, or `object` |
| `description` | becomes the flag's help text and the MCP argument description |
| `default` | used when the flag is omitted |
| `enum` | restricts the accepted values |
| `items` | element schema, for `array` |
| `format` | a passthrough hint, such as `date-time` |

Pass a complex input whole instead of flag by flag:

```sh
mytool update-thing --input payload.json
mytool update-thing --input-json '{"id":"42"}'
```

An explicitly passed flag wins over either, so a single value can be overridden on top of a
file.

---

## Versioning

```yaml
metadata:
  name: mytool
  version: 1.4.0

runtime:
  name: relay
  apiVersion: v1
```

`metadata.version` identifies your tool. `runtime` declares the Relay API the tool was
built against; installing a tool whose runtime the daemon cannot serve is refused with a
clear message rather than executed and left to fail in a confusing way.

A skill can pin itself to the manifest it ships with:

```markdown
---
version: 1.4.0
---
```

When present it must equal `metadata.version`, so a skill written for one version can
never be paired with the manifest of another.

---

## Configuration on disk

Two optional files under `~/.relay/config/` change daemon behaviour. Both are absent by
default, and both are read at startup — a malformed file is a hard error rather than a
policy silently ignored.

`permissions.yaml` sets the destructive-operation gate:

```yaml
destructive:
  - delete_repository
  - send_message
  - charge_customer
```

Omitting the file keeps the built-in default list above; an empty `destructive: []` is an
explicit opt-out.

`trust.yaml` is the local allowlist and blocklist used at install time:

```yaml
trusted:
  - name: github
    publisher: GitHub
blocked:
  - name: sketchy
```

A rule may pin `name`, `publisher`, `key`, or a combination. A `blocked` rule wins even
over a valid signature.

---

## See also

- [`../examples/`](../examples/) — worked manifests for each protocol, with the caveats
  each one carries.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — the build and verification loop.
- [`DESIGN.md`](DESIGN.md) — the full design spec, including the reasoning behind these
  rules.
