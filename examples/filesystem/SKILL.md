# Filesystem

Use this tool to work with files on the local machine: read a file, write a
file, enumerate a directory, and inspect a path's metadata. All four operations
act on the machine's own filesystem rather than a remote service.

## When to use this tool

Reach for it when the task is grounded in a local file or directory the user can
already name: reading a local config, writing an output artifact, or enumerating
a directory before deciding what to act on.

Do not use it as a substitute for a versioned or networked store. It touches the
filesystem directly and keeps no history, so a change it makes is a change to
the user's machine.

## Recommended workflows

Reading a file safely:

1. Use `list_directory` to confirm the path exists and to see its neighbors.
2. Use `read_file` on the confirmed path.
3. Choose the encoding deliberately; the default assumes text.

Editing a file:

1. Use `read_file` first and keep the original content.
2. Change only what the task requires.
3. Use `write_file` to save the result.

A compact discover-then-act sequence is:

`list_directory`
→ `read_file`
→ `write_file`

Use `stat` instead of `read_file` when only metadata is needed; it never loads
the file body.

## Choosing between operations

- `read_file` and `list_directory` sound similar but answer different
  questions. Use `list_directory` when you do not yet know which path to read;
  use `read_file` only on a path you have already identified.
- `stat` answers "what is this?" for one path — type, size, permission mode,
  and modification time — without reading content. Reach for it when the
  question is metadata rather than file contents.
- `write_file` overwrites. When a file may already exist, read it first so a
  write is a deliberate replacement rather than a surprise.
- `list_directory` is the only operation that can return many entries; the
  others act on exactly one path.

## Constraints

- Access is bounded by the tool's declared filesystem scopes. Reading a path —
  with `read_file`, `list_directory`, or `stat` — requires it to fall under a
  declared read scope; writing one requires a declared write scope. A path
  outside the declared scope is refused before any I/O happens.
- The path is resolved before it is checked: a leading tilde is expanded and
  symbolic links are followed, so a link pointing outside the declared scope is
  refused rather than followed.
- `write_file` replaces a file's entire content. There is no append and no
  partial edit.
- A single result is capped at 8 MiB. Reading a larger file, or listing a tree
  whose result would exceed the cap, is refused rather than truncated.
- Listing a large tree can be expensive; prefer a narrower path and a pattern,
  and leave recursion off when the top level is enough.

## Common mistakes

- Writing a binary payload as text. Request base64 encoding when the content is
  not UTF-8.
- Assuming a directory listing is a file list; directories and symbolic links
  appear as entries too.
- Treating a successful write as a new file. A file that already existed is
  reported as replaced, not created.
- Expecting a symbolic link to be traversed during a listing. Links are
  reported as their own entry type and never followed.
- Passing a relative path. Supply an absolute path or one that starts with a
  tilde.

## Pagination

Reading and listing do not paginate: a listing returns every matching entry, or
is refused for exceeding the output cap. Bound the result yourself instead —
scope the path, apply a pattern, and leave recursion off when the top level is
enough.

## Interpreting results

- `read_file` reports the path acted on, the encoding used, the byte size, and
  the content.
- `write_file` reports the path, the number of bytes written, and whether the
  file was created rather than replaced.
- `list_directory` reports the count and one entry per match, each with a name,
  path, type, and size.
- `stat` reports the type, size, permission mode, and modification time without
  content.
- Check the reported path against the one you asked for rather than assuming a
  call acted where you intended.

## Security boundary

The daemon, not this tool, is the security boundary. Before an operation runs,
the daemon derives the concrete path from the operation's input, resolves it,
and compares it against the declared scopes in the manifest. The executor
resolves the same path the same way, so the path that was authorized and the
path that is acted on are identical.
