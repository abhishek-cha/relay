// Package graphql implements the GraphQL executor behind the same
// protocol.Executor seam, so the CLI and MCP representations do not change.
// A tool whose manifest declares `protocol.type: graphql` posts one document —
// its `request.document`, with variables resolved from the operation input —
// to `protocol.endpoint` as {"query": ..., "variables": ...} (spec §19, §44).
//
// Two deliberate choices, both matching the REST executor's error taxonomy
// (spec §26):
//
//   - A GraphQL `errors` array in an HTTP 200 response becomes a structured
//     REMOTE_ERROR rather than being merged with whatever `data` came back.
//     Relay never hands an agent silently partial data; the caller can retry,
//     narrow the query, or surface the failure. The server's messages are
//     carried in the error details.
//   - On success the executor returns the response's `data` member, since that
//     is the payload an operation is declared for; `extensions` and the rest of
//     the envelope are dropped.
//
// The executor is stateless apart from its http.Client (which is safe for
// concurrent use), so a single instance serves concurrent invocations.
//
// See TASKS.md milestone M10.
package graphql
