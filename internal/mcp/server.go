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
		mcpTools = append(mcpTools, map[string]any{
			"name":        t.MCPName,
			"description": t.Description,
			"inputSchema": schema,
		})
	}

	s.sendResult(w, msg.ID, map[string]any{"tools": mcpTools})
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

	toolName, opName, err := s.resolveTool(ctx, params.Name)
	if err != nil {
		s.sendToolError(w, msg.ID, relay.NewError(relay.CodeToolNotFound, err.Error()))
		return
	}

	input := params.Arguments
	if input == nil {
		input = map[string]any{}
	}

	result, appErr := s.Invoker.Invoke(ctx, toolName, opName, input)
	if appErr != nil {
		s.sendToolError(w, msg.ID, appErr)
		return
	}

	payload, err := json.Marshal(result)
	if err != nil {
		s.sendToolError(w, msg.ID, relay.NewError(relay.CodeRemoteError,
			"failed to encode result: "+err.Error()))
		return
	}

	s.sendResult(w, msg.ID, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(payload)},
		},
		"isError": false,
	})
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

// resolveTool maps an MCP tool name back to its tool + operation pair by
// matching the cached descriptor catalog, which was itself derived from the
// binary's manifest.
func (s *Server) resolveTool(ctx context.Context, mcpName string) (toolName, opName string, err error) {
	tools, err := s.loadDescriptors(ctx)
	if err != nil {
		return "", "", err
	}
	for _, t := range tools {
		if t.MCPName == mcpName {
			return t.ToolName, t.OperationName, nil
		}
	}
	return "", "", fmt.Errorf("unknown tool: %s", mcpName)
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
