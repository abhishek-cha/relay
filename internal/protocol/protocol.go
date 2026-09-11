// Package protocol defines the execution seam between the daemon and whatever
// an external service actually speaks.
//
// REST is the first implementation, but nothing above this package may assume
// REST. The CLI, MCP adapter, and registry all deal in Relay operations; the
// manifest's `protocol.type` selects an Executor, and the protocol stays an
// implementation detail (spec §19).
package protocol

import "context"

// Executor runs a resolved Relay operation against a backend.
//
// Implementations: RESTExecutor, GraphQLExecutor, GRPCExecutor,
// BrowserExecutor, LocalExecutor. Only REST is planned for the MVP.
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
}

// Spec is the transport-neutral projection of a manifest request block.
type Spec struct {
	Type     string // rest | graphql | grpc | browser | local
	BaseURL  string
	Endpoint string
	Method   string
	Path     string
	Query    map[string]string
	Headers  map[string]string
	Body     any
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
}
