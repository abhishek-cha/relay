// Package rest implements the REST executor: the first protocol behind the
// protocol.Executor seam (spec §19, §20).
//
// Planned scope: GET/POST/PUT/PATCH/DELETE, path templating from input, query
// parameters, headers, JSON bodies, JSON responses, pagination, credential
// injection, and the structured error taxonomy from spec §26.
//
// See TASKS.md milestone M3.
package rest
