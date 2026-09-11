package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"relay/pkg/relay"
)

// ProtocolVersion is the MCP protocol revision this server advertises during
// the initialize handshake (spec §27). It is a real revision from the MCP
// specification rather than a private value, so a client can negotiate against
// it instead of guessing which framing it is talking to.
const ProtocolVersion = "2025-03-26"

// maxFrameBytes bounds one newline-delimited JSON-RPC frame. It matches the
// daemon's IPC limit, so a payload that survives the MCP hop survives the IPC
// hop too rather than failing only on the second leg.
const maxFrameBytes = 8 << 20

// Standard JSON-RPC 2.0 error codes. MCP reuses JSON-RPC framing, so these are
// protocol-level faults and are deliberately distinct from the structured relay
// errors that travel inside a tools/call result (spec §26, §29).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// rpcRequest is a decoded JSON-RPC 2.0 request or notification. ID is kept raw
// so a response echoes the client's id byte-for-byte whether it is a number, a
// string, or absent (a notification).
type rpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method"`
	Params  *json.RawMessage `json:"params,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response. Exactly one of Result or Error is set.
type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data optionally carries a structured relay error so a protocol-level
	// failure still exposes the §26 code (spec §26, §29).
	Data any `json:"data,omitempty"`
}

// ToolDescriptor describes one MCP-exposed tool with the input schema an MCP
// client needs to call it.
//
// It is a projection of the tool binary's manifest, never a schema source of
// its own: the binary stays authoritative and the registry stays discovery
// metadata (spec §16, §27). Keeping it a projection is what guarantees the CLI
// and MCP definitions cannot drift apart (spec §29).
type ToolDescriptor struct {
	// MCPName is the fully-qualified MCP tool name, "{tool}_{operation}"
	// (spec §28).
	MCPName string
	// ToolName is the registered tool name, e.g. "github".
	ToolName string
	// OperationName is the manifest operation, e.g. "get_repository".
	OperationName string
	// Description is the operation's human-readable description.
	Description string
	// InputSchema is the operation's JSON Schema, copied verbatim from the
	// manifest so MCP advertises exactly what the CLI accepts.
	InputSchema map[string]any
	// Paginated reports whether the operation's manifest declares a pagination
	// strategy (spec §20). It is what lets tools/list advertise the reserved
	// PaginateArg argument on exactly the operations that can act on it.
	Paginated bool
}

// DescriptorSource provides the MCP tool catalog. Each tool is described with
// its full input JSON Schema so tools/list can advertise the contract to MCP
// clients (spec §16). The binary remains authoritative for schemas; this
// interface is the narrow seam the adapter depends on, which is what lets the
// catalog be faked in tests without a daemon or installed tools.
type DescriptorSource interface {
	ListTools(ctx context.Context) ([]ToolDescriptor, error)
}

// Invoker executes one tool operation through the same path the CLI uses. The
// production implementation delegates to the daemon over the tool binary's IPC
// invoker (spec §27, §41). Returning a *relay.Error surfaces it as an MCP tool
// error carrying the identical code the CLI would print, rather than as a
// protocol-level fault.
type Invoker interface {
	Invoke(ctx context.Context, tool, operation string, input map[string]any) (any, *relay.Error)
}

// PaginateArg is the reserved MCP argument that opts a call into a paginated
// walk (spec §20). tools/list advertises it, as an optional boolean, only on
// operations whose manifest declares a pagination strategy, and the adapter
// consumes it as the walk opt-in rather than forwarding it as operation input.
//
// Precedence: the name is reserved only on a paginating operation. There the
// reserved meaning wins, so a manifest that also declared an input property
// literally named "paginate" cannot have that input set through MCP — the value
// is the walk opt-in and is stripped before the operation's input is validated.
// On an operation with no declared pagination the name is not reserved, the
// argument is not advertised, and a declared "paginate" input keeps its normal
// meaning. TestPaginateReservedNamePrecedence pins both halves.
const PaginateArg = "paginate"

// Invocation is one tool result plus the walk shape a paginated call reports
// (spec §20). Result is the machine result the CLI would print on stdout; Pages
// and Truncated are zero for an ordinary single-request call and are populated
// only when the caller opted into pagination, so a bounded walk is never
// mistaken for a complete collection.
type Invocation struct {
	Result    any
	Pages     int
	Truncated bool
}

// PaginatingInvoker is the optional extension of Invoker that carries the
// pagination opt-in. The adapter type-asserts for it instead of widening
// Invoker: the four-argument Invoke stays the compatibility contract, so every
// existing Invoker and its tests keep compiling, while a server whose daemon can
// walk pages advertises the reserved argument and forwards it (spec §27, §41).
type PaginatingInvoker interface {
	InvokePaginated(ctx context.Context, tool, operation string, input map[string]any, paginate bool) (Invocation, *relay.Error)
}

// ServerInfo identifies this MCP server to clients during the initialize
// handshake.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Server is a stdio MCP server: JSON-RPC 2.0 over newline-delimited
// stdin/stdout. stdout carries protocol frames only; every diagnostic goes to
// the Log writer, never stdout — the same stdout/stderr discipline tool
// binaries follow (spec §10). A stray log line on stdout would corrupt the
// stream for the client, so this is a correctness requirement, not a style
// preference.
//
// Serve is not safe for concurrent use: one stdio stream is one conversation.
type Server struct {
	Source  DescriptorSource
	Invoker Invoker
	// Skills exposes each registered tool's embedded SKILL.md as an MCP
	// resource (spec §8, §29). A nil source advertises no resources rather than
	// failing the method, so the server stays usable in discovery-only setups.
	Skills SkillSource
	Info   ServerInfo
	Log    io.Writer // receives non-fatal diagnostics; never stdout

	mu          sync.Mutex
	descriptors []ToolDescriptor
	descLoaded  bool
}

// Serve reads JSON-RPC frames from r, dispatches them, and writes responses to
// w. Protocol frames go exclusively to w; diagnostics go to s.Log. Serve
// returns when r is closed or on an unrecoverable read error.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg rpcRequest
		if err := json.Unmarshal(line, &msg); err != nil {
			// A frame the server cannot decode still deserves a decodable
			// answer; the id is unknowable, so it is null per JSON-RPC.
			s.sendError(w, nil, CodeParseError, "parse error")
			continue
		}

		s.dispatch(ctx, &msg, w)
	}

	return scanner.Err()
}

// dispatch routes one decoded message. Notifications (no id) are
// fire-and-forget: they never produce a response, because answering one would
// put an unsolicited frame on the wire.
func (s *Server) dispatch(ctx context.Context, msg *rpcRequest, w io.Writer) {
	notification := msg.ID == nil

	if msg.JSONRPC != "2.0" {
		if notification {
			s.logf("ignored frame with jsonrpc %q", msg.JSONRPC)
			return
		}
		s.sendError(w, msg.ID, CodeInvalidRequest, "jsonrpc must be \"2.0\"")
		return
	}

	if notification {
		// notifications/initialized and friends are acknowledgements; there is
		// nothing to answer. A real request sent without an id is worth a
		// diagnostic so it is not silently swallowed.
		if !strings.HasPrefix(msg.Method, "notifications/") {
			s.logf("ignored request without id: %s", msg.Method)
		}
		return
	}

	switch msg.Method {
	case "initialize":
		s.handleInitialize(msg, w)
	case "tools/list":
		s.handleToolsList(ctx, msg, w)
	case "tools/call":
		s.handleToolsCall(ctx, msg, w)
	case "resources/list":
		s.handleResourcesList(ctx, msg, w)
	case "resources/read":
		s.handleResourcesRead(ctx, msg, w)
	case "ping":
		s.handlePing(msg, w)
	case "":
		s.sendError(w, msg.ID, CodeInvalidRequest, "missing method")
	default:
		s.sendError(w, msg.ID, CodeMethodNotFound, "method not found: "+msg.Method)
	}
}

// handleInitialize completes the MCP handshake, advertising the protocol
// revision and the tools capability this server actually implements.
func (s *Server) handleInitialize(msg *rpcRequest, w io.Writer) {
	s.sendResult(w, msg.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		},
		"serverInfo": s.Info,
	})
}

// handleToolsList returns the tool catalog with full input schemas. The catalog
// is loaded once and cached: discovery is metadata from a local registry, so
// re-reading it per call would buy nothing while a client lists tools
// repeatedly.
func (s *Server) handleToolsList(ctx context.Context, msg *rpcRequest, w io.Writer) {
	tools, err := s.loadDescriptors(ctx)
	if err != nil {
		s.sendError(w, msg.ID, CodeInternalError, "failed to list tools: "+err.Error())
		return
	}

	mcpTools := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			// MCP requires an inputSchema on every tool; an operation with no
			// declared input still accepts an empty object.
			schema = map[string]any{"type": "object"}
		}
		schema = advertisedSchema(schema, t.Paginated)
		mcpTools = append(mcpTools, map[string]any{
			"name":        t.MCPName,
			"description": t.Description,
			"inputSchema": schema,
		})
	}

	s.sendResult(w, msg.ID, map[string]any{"tools": mcpTools})
}

// advertisedSchema returns the schema tools/list advertises for one operation.
// It is the manifest's schema verbatim except that a paginating operation gains
// one reserved, optional boolean property, PaginateArg. The manifest's own
// properties, and its required list, are copied through untouched, so the
// manifest stays the single source of truth and the CLI and MCP definitions
// cannot diverge (spec §7, §29). PaginateArg is never added to required: a walk
// is opt-in, so the default stays a single page.
func advertisedSchema(schema map[string]any, paginated bool) map[string]any {
	if !paginated {
		return schema
	}
	out := make(map[string]any, len(schema)+1)
	for key, value := range schema {
		out[key] = value
	}
	properties := make(map[string]any)
	if declared, ok := schema["properties"].(map[string]any); ok {
		for key, value := range declared {
			properties[key] = value
		}
	}
	properties[PaginateArg] = map[string]any{
		"type":        "boolean",
		"description": "Follow the operation's declared pagination and return the whole collection (opt-in; the default is a single page).",
	}
	out["properties"] = properties
	return out
}

// handleToolsCall resolves the tool and invokes it through the same daemon path
// the CLI uses (spec §27). Structured relay errors — including the
// NETWORK_ERROR a missing daemon produces — become an MCP tool error rather
// than a protocol fault, so the client sees exactly what the CLI would report
// instead of the server appearing to crash (spec §29).
func (s *Server) handleToolsCall(ctx context.Context, msg *rpcRequest, w io.Writer) {
	if msg.Params == nil {
		s.sendError(w, msg.ID, CodeInvalidParams, "missing params")
		return
	}

	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(*msg.Params, &params); err != nil {
		s.sendError(w, msg.ID, CodeInvalidParams, "invalid params: "+err.Error())
		return
	}
	if params.Name == "" {
		s.sendError(w, msg.ID, CodeInvalidParams, "missing tool name")
		return
	}

	desc, err := s.resolveDescriptor(ctx, params.Name)
	if err != nil {
		s.sendToolError(w, msg.ID, relay.NewError(relay.CodeToolNotFound, err.Error()))
		return
	}

	input := params.Arguments
	if input == nil {
		input = map[string]any{}
	}

	// PaginateArg is reserved only on an operation that declares pagination. On
	// such an operation it is the walk opt-in, so it is read here and stripped
	// before the daemon validates the operation input; it must never be forwarded
	// as an ordinary argument.
	paginate := false
	if desc.Paginated {
		if raw, present := input[PaginateArg]; present {
			flag, ok := raw.(bool)
			if !ok {
				s.sendToolError(w, msg.ID, relay.NewError(relay.CodeInvalidInput,
					fmt.Sprintf("%s must be a boolean", PaginateArg)))
				return
			}
			paginate = flag
		}
		delete(input, PaginateArg)
	}

	invocation, appErr := s.invokeTool(ctx, desc, input, paginate)
	if appErr != nil {
		s.sendToolError(w, msg.ID, appErr)
		return
	}

	payload, err := json.Marshal(invocation.Result)
	if err != nil {
		s.sendToolError(w, msg.ID, relay.NewError(relay.CodeRemoteError,
			"failed to encode result: "+err.Error()))
		return
	}

	result := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(payload)},
		},
		"isError": false,
	}
	if paginate {
		// Report the walk's shape inside the tool result, never as a stray stdout
		// line: stdout carries protocol frames only (spec §10), and a caller must
		// be able to tell a bounded walk from a complete collection (spec §20).
		result["pages"] = invocation.Pages
		result["truncated"] = invocation.Truncated
	}

	s.sendResult(w, msg.ID, result)
}

// invokeTool runs one resolved operation through the invoker. It prefers the
// optional PaginatingInvoker extension, so the walk opt-in reaches the daemon;
// an invoker that cannot carry it still serves ordinary calls unchanged. A
// caller that asked for a walk the invoker cannot perform is refused rather
// than handed a single page that would look complete (spec §20).
func (s *Server) invokeTool(ctx context.Context, desc ToolDescriptor, input map[string]any, paginate bool) (Invocation, *relay.Error) {
	if invoker, ok := s.Invoker.(PaginatingInvoker); ok {
		return invoker.InvokePaginated(ctx, desc.ToolName, desc.OperationName, input, paginate)
	}
	if paginate {
		return Invocation{}, relay.NewError(relay.CodeInvalidInput,
			"this server's invoker cannot paginate")
	}
	result, appErr := s.Invoker.Invoke(ctx, desc.ToolName, desc.OperationName, input)
	if appErr != nil {
		return Invocation{}, appErr
	}
	return Invocation{Result: result}, nil
}

// handlePing answers the liveness probe with an empty result.
func (s *Server) handlePing(msg *rpcRequest, w io.Writer) {
	s.sendResult(w, msg.ID, map[string]any{})
}

// loadDescriptors returns the cached tool descriptors, fetching them from the
// source on first access. A failed load is not cached, so a client can retry
// after fixing the underlying problem.
func (s *Server) loadDescriptors(ctx context.Context) ([]ToolDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.descLoaded {
		return s.descriptors, nil
	}
	if s.Source == nil {
		return nil, fmt.Errorf("no descriptor source is configured")
	}
	tools, err := s.Source.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	s.descriptors = tools
	s.descLoaded = true
	return tools, nil
}

// resolveDescriptor maps an MCP tool name back to the descriptor it was
// advertised from, matching the cached catalog, which was itself derived from
// the binary's manifest. The descriptor carries the operation's pagination flag
// so the call path knows whether PaginateArg is reserved.
func (s *Server) resolveDescriptor(ctx context.Context, mcpName string) (ToolDescriptor, error) {
	tools, err := s.loadDescriptors(ctx)
	if err != nil {
		return ToolDescriptor{}, err
	}
	for _, t := range tools {
		if t.MCPName == mcpName {
			return t, nil
		}
	}
	return ToolDescriptor{}, fmt.Errorf("unknown tool: %s", mcpName)
}

// sendResult writes a successful JSON-RPC response.
func (s *Server) sendResult(w io.Writer, id any, result any) {
	s.writeFrame(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// sendError writes a JSON-RPC protocol-level error response.
func (s *Server) sendError(w io.Writer, id any, code int, message string) {
	s.writeFrame(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

// sendToolError writes a successful JSON-RPC response whose result carries a
// relay structured error as an MCP tool error. The error is serialized verbatim
// so a client can branch on the same code the CLI would print (spec §26, §29).
func (s *Server) sendToolError(w io.Writer, id any, err *relay.Error) {
	payload, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		s.logf("failed to encode structured error: %v", marshalErr)
		payload = []byte(`{"code":"REMOTE_ERROR"}`)
	}
	s.writeFrame(w, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(payload)},
			},
			"isError": true,
		},
	})
}

// writeFrame encodes one response as a single newline-terminated JSON line.
// Encoding or write failures go to the log writer, because stdout must never
// carry anything but frames.
func (s *Server) writeFrame(w io.Writer, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		s.logf("failed to encode JSON-RPC frame: %v", err)
		return
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		s.logf("failed to write JSON-RPC frame: %v", err)
	}
}

// logf writes a formatted diagnostic to the log writer.
func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}
