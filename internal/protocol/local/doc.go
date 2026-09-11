// Package local implements the LocalExecutor behind the same
// protocol.Executor seam the REST and GraphQL protocols use, so a tool that
// packages a local capability reaches an agent through the identical daemon
// path as a remote API — no CLI or MCP change (spec §19, §27, §46).
//
// A local tool does not talk to a service. Its manifest declares
// `protocol.type: local` and each operation's request block names the local
// primitive it invokes (the YAML is in the package's example manifest and in
// docs/DESIGN.md §46):
//
//	request:
//	  operation: read_file
//
// This build implements two families (spec §46):
//
//   - filesystem — read_file, write_file, list_directory, stat. These call the
//     Go standard library in-process; no process is spawned.
//   - git — git_status, git_diff, git_log. These invoke the `git` binary as a
//     read-only inspection of a working tree. The binary is executed directly,
//     never through a shell, and optional index locks and external diff/textconv
//     helpers are disabled so inspection cannot mutate the repository.
//
// The daemon remains the security boundary (spec §24, §40). The executor does
// not decide whether an operation may touch a path: the daemon derives the
// concrete path from the operation input through [Requirements], resolves it
// through [ResolvePath], and checks it against the manifest's declared
// permissions.filesystem scopes before the executor is called. The executor
// resolves the target by the same rule, so the path that was checked is the
// path that is acted on.
//
// A local operation's transport projection onto [protocol.Spec] is:
//
//	Type     = "local"
//	Endpoint = the request block's `operation` — the primitive to run
//	BaseURL, Method, Path, Query, Headers, Body = empty
//
// Endpoint is the Spec's protocol-specific destination slot (a URL for
// GraphQL); local has no address, so it carries the capability's name.
//
// See TASKS.md milestone M10.
package local
