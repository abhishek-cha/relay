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
}

// InvokeResponse is the daemon's reply. Exactly one of Result or Error is set.
type InvokeResponse struct {
	Success bool   `json:"success"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
}
