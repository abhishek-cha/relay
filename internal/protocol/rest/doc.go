// Package rest implements the REST executor: the first protocol behind the
// protocol.Executor seam (spec §19, §20).
//
// Scope: GET/POST/PUT/PATCH/DELETE, path templating from input, query
// parameters, headers, JSON bodies, JSON responses, credential injection, the
// structured error taxonomy from spec §26, and manifest-declared pagination
// (spec §20). Pagination follows a Link header or a cursor only when a caller
// opts in, and is bounded by both a page cap and a total-byte cap.
//
// See TASKS.md milestone M3.
package rest
