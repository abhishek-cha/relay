package ipc

import "relay/pkg/relay"

// ConfirmationField is the JSON key an invoke frame uses to carry an explicit
// acknowledgment for one destructive operation (spec §25).
//
// The field lives beside the transport, like the other daemon-only frame data:
// the daemon and the calling surface share this package, while pkg/relay stays
// the public contract spoken by tool binaries. A caller with nothing to confirm
// simply omits the key, so an unconfirmed invoke frame is exactly the wire shape
// that predates this feature.
const ConfirmationField = "confirmation"

// InvokeFrame is the invoke wire shape: the shared relay.InvokeRequest plus the
// optional confirmation acknowledgment.
//
// Embedding relay.InvokeRequest anonymously flattens its fields, so a frame with
// no confirmation is byte-for-byte the request a tool binary always sent. The
// daemon decodes every invoke through this type; the tool runtime and the MCP
// adapter keep sending plain relay.InvokeRequest, which decodes to an empty
// Confirmation and is therefore never treated as confirmed (spec §41).
type InvokeFrame struct {
	relay.InvokeRequest
	Confirmation string `json:"confirmation,omitempty"`
}

// ConfirmationToken renders the explicit acknowledgment a caller echoes back to
// run one destructive operation (spec §25).
//
// The token binds the acknowledgment to the exact tool and operation, so a
// confirmation for one operation can never resolve the gate for another. It is
// deliberately not a secret and not a credential: it carries no information the
// caller did not already state, so it is safe to rebuild on demand and must never
// be persisted. The security property is structural rather than cryptographic —
// only a surface that prompted a human sends it, and the MCP adapter has no field
// through which to send one (spec §41).
func ConfirmationToken(tool, operation string) string {
	return "confirm:" + tool + ":" + operation
}

// Confirms reports whether confirmation is the token for this exact tool and
// operation. An empty confirmation never confirms anything.
func Confirms(confirmation, tool, operation string) bool {
	return confirmation != "" && confirmation == ConfirmationToken(tool, operation)
}
