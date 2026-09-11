# Filesystem

Use this tool to work with files on the local machine: read a file, write a
file, list a directory, and delete a path.

## Execution status

Read this before relying on the tool.

This tool builds, and its `--describe` and `--skill` contracts work, but it does
**not** execute yet. Its protocol is `local`, and Relay ships only the REST
executor today; there is no local executor until milestone M10 (spec 46). Every
operation currently fails with `PROTOCOL_ERROR`.

Treat this example as the intended shape of a local capability, not as a working
filesystem tool. The manifest and this skill describe the model so it can be
reviewed and implemented; they do not perform I/O.

## When to use this tool

Once the local executor exists, reach for it when a task needs the machine's own
files rather than a remote service: reading a local config, writing an output
artifact, or enumerating a directory before deciding what to act on.

Do not use it as a substitute for a versioned or networked store. It touches the
filesystem directly and has no history, so a change it makes is a change to the
user's machine.

## Recommended workflows

Reading a file safely:

1. Use `list_directory` to confirm the path exists and to see its neighbors.
2. Use `read_file` on the confirmed path.
3. Choose `encoding` deliberately; the default assumes text.

Editing a file:

1. Use `read_file` first and keep the original content.
2. Change only what the task requires.
3. Use `write_file` to save the result.

A compact discover-then-act sequence is:

`list_directory`
→ `read_file`
→ `write_file`

## Choosing between operations

- `read_file` and `list_directory` sound similar but answer different questions. Use `list_directory` when you do not yet know which path to read; use `read_file` only on a path you have identified.
- `write_file` overwrites. When a file may already exist, read it first so a write is a deliberate replacement rather than a surprise.
- `delete_file` is the only destructive operation here. Everything else is reversible by re-reading or re-writing; deletion is not.

## Constraints

- Access is bounded by the tool's declared filesystem permissions. Paths outside the permitted roots should be refused rather than attempted.
- `delete_file` has no undo. Without a confirmation step (spec 25), treat it as permanent and require explicit intent before calling it.
- `write_file` replaces a file's entire content. There is no append and no partial edit.
- Listing a large tree can be expensive; prefer a narrower `path` and a `pattern` over a broad recursive listing.

## Common mistakes

- Assuming the tool runs. It does not until the local executor lands; every call returns `PROTOCOL_ERROR`.
- Deleting before reading. Read the target first so the action is informed.
- Writing a binary payload as text. Use base64 when the content is not UTF-8.
- Treating a directory listing as a file list; directories appear as entries too.

## Pagination

Local listing does not paginate. Bound the result yourself instead: scope the
`path`, apply a `pattern`, and avoid `recursive` when the top level is enough.

## Interpreting results

Because the tool does not execute yet, there is currently no result to interpret
— a call returns a structured `PROTOCOL_ERROR` rather than data. When the local
executor is implemented, expect a result that reports the path acted on and the
bytes or entries involved, and check it against the request rather than assuming
success from a zero exit alone.

## Manifest status

The `request` blocks in the manifest are nominal placeholders. The current
relay/v1 validator requires a method and path on every operation, so they are
present to satisfy validation, not to describe HTTP calls. A future local
executor would read the operation's inputs directly.
