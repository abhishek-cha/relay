# Examples

Example tool projects, one directory per tool. Each is the `github.yaml` +
`SKILL.md` pair from spec §42 and §43:

```
examples/
├── github/
│   ├── github.yaml   # the manifest (machine truth)
│   └── SKILL.md      # the skill (LLM guidance)
├── slack/
│   └── slack.yaml    # draft
└── stripe/
    └── stripe.yaml   # draft
```

`github/` is the canonical, spec-derived example and is what the end-to-end test
in spec §60 builds.

`slack/` and `stripe/` are **illustrative drafts**: the endpoints shown are real
and well-known, but they have not been exercised against the live APIs and each
needs a `SKILL.md` before it is useful to an agent.
