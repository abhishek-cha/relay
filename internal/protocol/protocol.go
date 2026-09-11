// Package protocol defines the execution seam between the daemon and whatever
// an external service actually speaks.
//
// REST, GraphQL, gRPC, browser, and local each have an Executor behind this
// seam, but nothing above this package may assume any one of them. The CLI,
// MCP adapter, and registry all deal in Relay operations; the manifest's
// `protocol.type` selects an Executor, and the protocol stays an
// implementation detail (spec §19).
package protocol

import "context"

// Executor runs a resolved Relay operation against a backend.
//
// Implementations: RESTExecutor, GraphQLExecutor, GRPCExecutor,
// BrowserExecutor, LocalExecutor.
type Executor interface {
	Execute(ctx context.Context, req Request) (Response, error)
}

// Request is a protocol-agnostic execution request, already resolved against
// the operation's manifest entry by the daemon.
type Request struct {
	Tool      string
	Operation string
	Input     map[string]any

	// Credential is injected by the daemon after auth resolution. It is never
	// logged, echoed, or persisted (spec §22, §40).
	Credential *Credential

	// Spec is the operation's resolved transport description.
	Spec Spec

	// Paginate turns on page-following for a REST operation whose manifest
	// declared a pagination strategy (spec §20). It is opt-in: the daemon sets
	// it from the caller's request, so a declared strategy is inert until a
	// caller asks for the whole collection. It has no effect when Spec.Pagination
	// is nil.
	Paginate bool
}

// Spec is the transport-neutral projection of a manifest request block.
type Spec struct {
	Type     string // rest | graphql | grpc | browser | local
	BaseURL  string
	Endpoint string
	// Method and Path are the REST HTTP verb and URL path. For a gRPC operation
	// addressed by name (spec §45), Package and Service carry the service and
	// Method carries the RPC method name, while Path stays empty; a gRPC
	// operation addressed by path leaves Package and Service empty and puts the
	// canonical /package.Service/Method in Path, as before.
	Method  string
	Path    string
	Package string
	Service string
	Query   map[string]string
	Headers map[string]string
	Body    any

	// Document is the GraphQL operation text (a query or mutation) and
	// Variables maps a GraphQL variable name to the operation input property
	// that supplies it (spec §44). Both stay empty for every other protocol,
	// so a REST spec is unchanged.
	Document  string
	Variables map[string]string

	// Pagination is the manifest's declared page-following strategy (spec §20).
	// It is set only for the protocol that declared one and stays nil for every
	// other operation, so an operation without pagination executes exactly as a
	// single request.
	Pagination *Pagination
}

// Pagination is the transport-neutral projection of a manifest pagination
// block (spec §20). Style is "link-header" or "cursor"; the remaining fields
// carry meaning only for the style that reads them, matching the manifest.
type Pagination struct {
	Style        string
	CursorParam  string
	CursorIn     string
	CursorField  string
	HasMoreField string
	LimitParam   string
	Limit        int
}

// Credential is a resolved secret plus how to present it. Keep the secret
// opaque to everything except the protocol executor that needs it.
type Credential struct {
	Type   string // api_key | bearer | basic | oauth2 | client_credentials
	Secret string
	Header string
	Scheme string
}

// Response is a normalized backend response.
type Response struct {
	Status  int
	Headers map[string][]string
	Body    any

	// Pages is the number of backend pages a paginated operation collected. A
	// single-request operation reports 1. Truncated is true when page-following
	// stopped at an executor cap while the backend still offered a next page, so
	// a caller can tell a complete collection from a partial one rather than
	// reading a bounded walk as the whole result (spec §20).
	Pages     int
	Truncated bool
}
