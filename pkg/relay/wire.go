package relay

// Wire types shared by the tool runtime, the daemon, and the MCP adapter.
// These mirror the JSON contracts in specs §9, §14, §16, and §35.

// Descriptor is the primary discovery and registration contract: the JSON a
// Relay tool prints for `--describe` (spec §9).
//
// The tool binary is authoritative for its descriptor. The registry stores it
// only as discovery metadata and must never serve it as a schema source of
// truth (spec §16).
type Descriptor struct {
	APIVersion   string        `json:"apiVersion"`
	Kind         string        `json:"kind"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	Description  string        `json:"description,omitempty"`
	Protocol     string        `json:"protocol"`
	Runtime      RuntimeInfo   `json:"runtime"`
	Skill        bool          `json:"skill"`
	Capabilities []string      `json:"capabilities,omitempty"`
	Tools        []ToolSummary `json:"tools"`
}

// ToolSummary is the per-operation discovery record.
type ToolSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// RuntimeInfo declares which Relay runtime a tool requires, so incompatible
// tools are rejected cleanly rather than executed (spec §35).
type RuntimeInfo struct {
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion"`
}

// InvokeRequest is a tool invocation sent from a tool binary to the daemon over
// the Unix socket (spec §14).
type InvokeRequest struct {
	Type      string         `json:"type"` // always "invoke"
	Tool      string         `json:"tool"`
	Operation string         `json:"operation"`
	Input     map[string]any `json:"input"`

	// Paginate asks the daemon to follow a REST operation's declared pagination
	// strategy and return the whole collection (spec §20). It is additive and
	// opt-in: an omitted field stays false, so the request a caller sends by
	// default is byte-for-byte the one that predates pagination, and an
	// operation with no declared strategy always runs as a single request.
	Paginate bool `json:"paginate,omitempty"`
}

// InvokeResponse is the daemon's reply. Exactly one of Result or Error is set.
type InvokeResponse struct {
	Success bool   `json:"success"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`

	// Pages and Truncated report how a paginated request was satisfied (spec
	// §20). They are set only when the caller opted in; an ordinary invocation
	// leaves both at their zero values and therefore unchanged on the wire.
	// Truncated is true when the walk stopped at an executor cap while more pages
	// remained, so a bounded walk is never mistaken for the whole collection.
	Pages     int  `json:"pages,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}
