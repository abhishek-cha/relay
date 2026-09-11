# <Tool>

Use this tool to <one sentence in the user's terms — what capability it adds,
not how it is implemented>.

## When to use this tool

<The situations where this tool is the right choice. Name the trigger in the
user's request that should make an agent reach for it. Then say where it is the
wrong choice, so an agent does not stretch it into a task it is poor at.>

## Recommended workflows

<The operation ordering that usually works, as a numbered sequence with a short
reason on each step. Start from the cheapest call that narrows the problem.>

1. <First operation — what it establishes.>
2. <Second operation — what it does with that.>
3. <Third operation — what it produces.>

A compact sequence is:

`<first_operation>`
→ `<second_operation>`
→ `<third_operation>`

## Choosing between operations

<Which operation to reach for when two look similar. Contrast them by the
question each answers, and give the deciding rule in one sentence. Name the
wrong-looking-right choice so an agent does not make it.>

## Constraints

<The limits an agent must respect: ordering rules, writes versus reads,
irreversible actions, rate limits, scope boundaries, and anything the tool
refuses to do. State consequences, not just rules.>

## Common mistakes

<What agents get wrong with this tool, phrased as the mistake and the fix. Keep
each item to one line.>

- <Mistake — the correction.>
- <Mistake — the correction.>

## Pagination

<How to page through results when the backing service paginates: which field
carries the cursor or offset, how to pass it back, and when to stop. If results
are not paginated, say so and describe how to bound the result instead.>

## Interpreting results

<What the response means and which parts matter. Call out any application-level
success signal that is separate from the transport result, units, timestamp
formats, and fields that are easy to misread.>

---

This template is guidance, not a schema. Do not restate the manifest's inputs or
outputs here — a build-time linter rejects skills that do (spec §30). The
manifest is machine truth; this file is how an agent should use it.
