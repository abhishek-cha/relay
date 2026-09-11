package relay

import "time"

// IPC frame kinds on the daemon's local surface (spec §14).
//
// The wire format is newline-delimited JSON: one flat object per line,
// discriminated by its "type" field. The frame set is deliberately small and
// free of Go-specific encoding, because the daemon is the piece another
// language's runtime would have to speak (spec §5).
const (
	FrameHello    = "hello"
	FrameInvoke   = "invoke"
	FrameList     = "list"
	FrameInspect  = "inspect"
	FrameStatus   = "status"
	FrameRegister = "register"
	FrameRemove   = "remove"
)

// Runtime identity. A tool declares the runtime contract it needs; the daemon
// declares the one it provides. A mismatch is rejected cleanly rather than
// guessed at (spec §35).
const (
	RuntimeName       = "relay"
	RuntimeAPIVersion = "v1"
)

// ClientInfo identifies a caller during the hello handshake.
type ClientInfo struct {
	Name              string `json:"name"`
	Version           string `json:"version,omitempty"`
	RuntimeAPIVersion string `json:"runtimeApiVersion,omitempty"`
}

// ServerInfo identifies the running daemon.
type ServerInfo struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Pid       int       `json:"pid"`
	Socket    string    `json:"socket"`
	StartedAt time.Time `json:"startedAt"`
}

// HelloRequest is the version handshake. A client states the runtime contract
// it was built against so a mismatch surfaces as RUNTIME_INCOMPATIBLE instead
// of a confusing downstream failure (spec §35).
type HelloRequest struct {
	Type   string     `json:"type"`
	Client ClientInfo `json:"client"`
}

// HelloResponse reports the daemon's identity and the tools it can serve.
type HelloResponse struct {
	Success bool           `json:"success"`
	Error   *Error         `json:"error,omitempty"`
	Server  ServerInfo     `json:"server"`
	Runtime RuntimeInfo    `json:"runtime"`
	Tools   []Installation `json:"tools"`
}

// ListRequest asks for the installed tools.
type ListRequest struct {
	Type string `json:"type"`
}

// ListResponse carries registry records.
type ListResponse struct {
	Success bool           `json:"success"`
	Error   *Error         `json:"error,omitempty"`
	Tools   []Installation `json:"tools"`
}

// InspectRequest asks for one tool's descriptor.
type InspectRequest struct {
	Type string `json:"type"`
	Tool string `json:"tool"`
}

// InspectResponse carries a tool's own descriptor plus its registry record.
type InspectResponse struct {
	Success    bool          `json:"success"`
	Error      *Error        `json:"error,omitempty"`
	Descriptor *Descriptor   `json:"descriptor,omitempty"`
	Install    *Installation `json:"install,omitempty"`
}

// StatusRequest asks the daemon to report its health (spec §3.3).
type StatusRequest struct {
	Type string `json:"type"`
}

// StatusResponse is the daemon's health record.
type StatusResponse struct {
	Success       bool        `json:"success"`
	Error         *Error      `json:"error,omitempty"`
	Server        ServerInfo  `json:"server"`
	Runtime       RuntimeInfo `json:"runtime"`
	Tools         int         `json:"tools"`
	UptimeSeconds float64     `json:"uptimeSeconds"`
}

// RegisterRequest installs a tool binary (spec §15).
type RegisterRequest struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// RemoveRequest unregisters a tool.
type RemoveRequest struct {
	Type string `json:"type"`
	Tool string `json:"tool"`
}

// Installation is the registry record (spec §16).
//
// It is discovery metadata only. It deliberately does not carry operations,
// input schemas, or request details: the tool binary stays authoritative for
// its schema, and the daemon re-reads the manifest from the binary at
// execution time. Caching the schema here would create a second source of
// truth, which is the one thing §16 forbids.
type Installation struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Path        string    `json:"path"`
	InstalledAt time.Time `json:"installedAt"`
	Runtime     string    `json:"runtime"`
	// The remaining fields are recorded for display and drift detection.
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Skill      bool   `json:"skill,omitempty"`
	// BinaryHash is the SHA-256 of the installed binary, so a binary swapped
	// after registration is visible rather than silently trusted.
	BinaryHash string `json:"binaryHash,omitempty"`
	// Operations is the descriptor's operation list, kept so `relay list` can
	// render a summary without executing every installed binary.
	Operations []string `json:"operations,omitempty"`
}

// MutationResponse reports the outcome of a registry mutation.
type MutationResponse struct {
	Success bool          `json:"success"`
	Error   *Error        `json:"error,omitempty"`
	Tool    string        `json:"tool,omitempty"`
	Message string        `json:"message,omitempty"`
	Install *Installation `json:"install,omitempty"`
}
