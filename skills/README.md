# Skills

Reusable skill assets and templates live here.

A Relay tool's own skill is `SKILL.md`, authored next to its manifest and
embedded into the built binary at `relay build` time (spec §8, §42). This
directory is for anything shared across tools — starting points, conventions,
and (later) composite skills that describe how to combine several Relay tools.

Rules that apply to every skill:

- A skill is **LLM guidance**, not a second schema. Never restate the manifest's
  inputs or outputs (spec §30).
- Focus on usage patterns, workflow sequences, constraints, choosing between
  operations, common mistakes, pagination, and interpreting results.
- The manifest and its skill are **versioned together**; a skill written for one
  manifest version must not be paired with another (spec §36).

See [`../templates/SKILL.md`](../templates/SKILL.md) for a starting point.
