# Examples

Example tool projects, one directory per tool, in the same shape as the
`templates/` pair: `<tool>.yaml` is the machine truth and `SKILL.md` is the LLM
guidance.

| Example | Protocol | Skill | Status |
| --- | --- | --- | --- |
| `github/` | REST | yes | the canonical, spec-derived pair; runs against a real credential |
| `slack/` | REST | yes | real input schemas and bearer auth; not exercised against the live API |
| `browser/` | browser | yes | form login into a service with no API; not exercised live |
| `stripe/` | REST | no | manifest-only draft; builds with a "no skill" warning |

Build any of them from the repository root:

```sh
relay build examples/<name>/<name>.yaml --skill examples/<name>/SKILL.md
```

`--skill` is omitted only for `stripe/`, which has no `SKILL.md`.

Worth knowing per example:

- `github/` is the pair the end-to-end suite (`scripts/e2e.sh`) builds. It
  declares OAuth auth, so invoking an operation needs a stored credential
  (`relay auth login github`) and returns `AUTH_REQUIRED` without one.
- `slack/` demonstrates a caveat a skill should carry honestly: Slack answers
  HTTP 200 even when the method failed, so an agent has to read the `ok` field
  rather than trust the transport result.
- `browser/` models a service with no usable API. Relay logs in once with the
  credential from the Keychain, keeps the cookie in the daemon's session store,
  and attaches it to later operations; the operations themselves stay plain
  HTTP. This is the example to copy when a service is only reachable through a
  login form.
- `stripe/` is a manifest-only draft. Building it without `--skill` succeeds with
  a warning, and the binary's descriptor reports that it carries no skill. Its
  endpoint matches Stripe's documented API but has not been exercised live.

The full manifest shape for every protocol and credential type is documented in
[`docs/CAPABILITIES.md`](../docs/CAPABILITIES.md).
