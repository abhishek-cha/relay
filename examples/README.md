# Examples

Example tool projects, one directory per tool, in the same shape as the
`templates/` pair: `<tool>.yaml` is the machine truth, and `SKILL.md` is the
LLM guidance. Every example here is buildable with the command below; only the
`filesystem` one is not yet executable.

| Example | Protocol | Skill | Status |
| --- | --- | --- | --- |
| `github/` | REST | yes | the canonical, spec-derived pair; runs with a credential |
| `slack/` | REST | yes | real schemas and bearer auth; not exercised against the live API |
| `filesystem/` | local | yes | builds, but not executable — Relay has no local executor yet |
| `stripe/` | REST | no | manifest-only draft; builds with a "no skill" warning |

Build any of them from the repository root:

```sh
relay build examples/<name>/<name>.yaml --skill examples/<name>/SKILL.md
```

`--skill` is omitted only for `stripe/`, which has no `SKILL.md`.

Worth knowing per example:

- `github/` is what the end-to-end test (`scripts/e2e.sh`) builds. It declares
  OAuth auth, so invoking an operation needs a stored credential
  (`relay auth login github`) and returns `AUTH_REQUIRED` without one.
- `slack/` demonstrates the honest caveat a skill should carry: Slack answers
  HTTP 200 even when a method failed, so an agent has to read the `ok` field
  rather than trust the transport result.
- `filesystem/` models a local capability (`protocol.type: local`) instead of a
  remote API. It builds and its `--describe` and `--skill` contracts work, but
  Relay ships only the REST and GraphQL executors, so every operation fails
  with `PROTOCOL_ERROR` until a local executor exists (spec §19, §46).
- `stripe/` is a manifest-only draft. Building it without `--skill` succeeds
  with a warning, and the binary's descriptor reports that it carries no skill.
  Its endpoint matches Stripe's documented API but has not been exercised
  live.
