package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"relay/internal/manifest"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// --- fakes ---------------------------------------------------------------

type stubSource struct {
	tools []ToolDescriptor
	err   error
}

func (f *stubSource) ListTools(context.Context) ([]ToolDescriptor, error) {
	return f.tools, f.err
}

type stubInvoker struct {
	result    any
	err       *relay.Error
	called    bool
	lastTool  string
	lastOp    string
	lastInput map[string]any
}

func (f *stubInvoker) Invoke(_ context.Context, tool, op string, in map[string]any) (any, *relay.Error) {
	f.called = true
	f.lastTool = tool
	f.lastOp = op
	f.lastInput = in
	return f.result, f.err
}

type fakeOps struct {
	req  relay.InvokeRequest
	resp relay.InvokeResponse
	err  error
}

func (f *fakeOps) Invoke(_ context.Context, req relay.InvokeRequest) (relay.InvokeResponse, error) {
	f.req = req
	return f.resp, f.err
}

// --- helpers -------------------------------------------------------------

func newServer(src DescriptorSource, inv Invoker) *Server {
	return &Server{
		Source:  src,
		Invoker: inv,
		Info:    ServerInfo{Name: "relay-test", Version: "0.0.1"},
		Log:     io.Discard,
	}
}

func githubDescriptors() []ToolDescriptor {
	return []ToolDescriptor{{
		MCPName:       "github_get_repository",
		ToolName:      "github",
		OperationName: "get_repository",
		Description:   "Get repository information",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"repo":  map[string]any{"type": "string"},
			},
			"required": []any{"owner", "repo"},
		},
	}}
}

// serve drives frames through Serve and returns raw stdout plus parsed
// responses. It asserts the framing property on every test: stdout is nothing
// but valid JSON-RPC 2.0 lines.
func serve(t *testing.T, s *Server, frames ...string) (string, []rpcResponse) {
	t.Helper()
	input := strings.Join(frames, "\n") + "\n"
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	raw := out.String()
	responses := assertOnlyJSONRPC(t, raw)
	return raw, responses
}

// assertOnlyJSONRPC returns an error unless every non-empty stdout line is a
// JSON-RPC 2.0 frame. It returns the parsed responses.
func assertOnlyJSONRPC(t *testing.T, raw string) []rpcResponse {
	t.Helper()
	var responses []rpcResponse
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
	lines := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines++
		var envelope map[string]any
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("stdout line %d is not JSON: %q: %v", lines, line, err)
		}
		if envelope["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line %d is not a JSON-RPC 2.0 frame: %q", lines, line)
		}
		if _, ok := envelope["id"]; !ok {
			t.Fatalf("stdout line %d has no id field: %q", lines, line)
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("stdout line %d did not decode as a response: %v", lines, err)
		}
		responses = append(responses, resp)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan stdout: %v", err)
	}
	return responses
}

func resultMap(t *testing.T, resp rpcResponse) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("expected a result, got error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	m, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want object: %#v", resp.Result, resp.Result)
	}
	return m
}

func contentText(t *testing.T, resp rpcResponse) string {
	t.Helper()
	m := resultMap(t, resp)
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no content array: %#v", m)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content entry is %T", content[0])
	}
	text, _ := first["text"].(string)
	return text
}

func requireOne(t *testing.T, responses []rpcResponse) rpcResponse {
	t.Helper()
	if len(responses) != 1 {
		t.Fatalf("expected exactly 1 response, got %d", len(responses))
	}
	return responses[0]
}

// --- handshake + discovery ----------------------------------------------

func TestInitializeHandshake(t *testing.T) {
	s := newServer(&stubSource{tools: githubDescriptors()}, &stubInvoker{})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	)

	resp := requireOne(t, responses)
	m := resultMap(t, resp)
	if m["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %q", m["protocolVersion"], ProtocolVersion)
	}
	caps, ok := m["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is %T", m["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("tools capability not advertised: %#v", caps)
	}
	info, ok := m["serverInfo"].(map[string]any)
	if !ok || info["name"] != "relay-test" {
		t.Fatalf("serverInfo = %#v", m["serverInfo"])
	}
}

func TestToolsListExposesNameDescriptionSchema(t *testing.T) {
	s := newServer(&stubSource{tools: githubDescriptors()}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	m := resultMap(t, requireOne(t, responses))
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", m["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "github_get_repository" {
		t.Fatalf("tool name = %v", tool["name"])
	}
	if tool["description"] != "Get repository information" {
		t.Fatalf("description = %v", tool["description"])
	}
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("inputSchema = %#v", tool["inputSchema"])
	}
	required, _ := schema["required"].([]any)
	if len(required) != 2 {
		t.Fatalf("required = %#v", schema["required"])
	}
}

func TestToolsListDefaultsMissingSchema(t *testing.T) {
	s := newServer(&stubSource{tools: []ToolDescriptor{{
		MCPName:       "x_op",
		ToolName:      "x",
		OperationName: "op",
	}}}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)

	m := resultMap(t, requireOne(t, responses))
	tool := m["tools"].([]any)[0].(map[string]any)
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("inputSchema = %#v, want default object", tool["inputSchema"])
	}
}

func TestToolsListSourceErrorIsProtocolError(t *testing.T) {
	s := newServer(&stubSource{err: errors.New("registry unavailable")}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)

	resp := requireOne(t, responses)
	if resp.Error == nil || resp.Error.Code != CodeInternalError {
		t.Fatalf("error = %#v, want %d", resp.Error, CodeInternalError)
	}
}

// --- invocation ----------------------------------------------------------

func TestToolsCallInvokesThroughInvoker(t *testing.T) {
	inv := &stubInvoker{result: map[string]any{"name": "relay", "stars": 42}}
	s := newServer(&stubSource{tools: githubDescriptors()}, inv)
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"github_get_repository","arguments":{"owner":"openai","repo":"relay"}}}`,
	)

	resp := requireOne(t, responses)
	if !inv.called {
		t.Fatal("invoker was not called")
	}
	if inv.lastTool != "github" || inv.lastOp != "get_repository" {
		t.Fatalf("invoked %s/%s, want github/get_repository", inv.lastTool, inv.lastOp)
	}
	if inv.lastInput["owner"] != "openai" || inv.lastInput["repo"] != "relay" {
		t.Fatalf("input = %#v", inv.lastInput)
	}

	m := resultMap(t, resp)
	if m["isError"] != false {
		t.Fatalf("isError = %v, want false", m["isError"])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(contentText(t, resp)), &got); err != nil {
		t.Fatalf("content is not JSON: %v", err)
	}
	// The result travels as JSON text, so compare against the expected value
	// after the same encode/decode round-trip (numbers become float64).
	expected := roundTrip(t, inv.result)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("content = %#v, want %#v", got, expected)
	}
}

// roundTrip normalizes a value the way a JSON text payload would, so tests can
// compare a Go literal against what actually came off the wire.
func roundTrip(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestToolsCallUnknownToolIsToolError(t *testing.T) {
	s := newServer(&stubSource{tools: githubDescriptors()}, &stubInvoker{})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope_op","arguments":{}}}`,
	)

	resp := requireOne(t, responses)
	if resp.Error != nil {
		t.Fatalf("unknown tool must be a tool error, not a protocol error: %#v", resp.Error)
	}
	m := resultMap(t, resp)
	if m["isError"] != true {
		t.Fatalf("isError = %v, want true", m["isError"])
	}
	var structured relay.Error
	if err := json.Unmarshal([]byte(contentText(t, resp)), &structured); err != nil {
		t.Fatalf("tool error is not a relay.Error: %v", err)
	}
	if structured.Code != relay.CodeToolNotFound {
		t.Fatalf("code = %q, want %q", structured.Code, relay.CodeToolNotFound)
	}
}

func TestToolsCallDaemonUnavailableSurfacesNetworkError(t *testing.T) {
	inv := &stubInvoker{err: relay.NewError(relay.CodeNetworkError,
		"the Relay daemon is not available; start it with 'relay daemon start'")}
	s := newServer(&stubSource{tools: githubDescriptors()}, inv)
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"github_get_repository","arguments":{"owner":"o","repo":"r"}}}`,
	)

	resp := requireOne(t, responses)
	if resp.Error != nil {
		t.Fatalf("daemon absence must not be a protocol fault: %#v", resp.Error)
	}
	m := resultMap(t, resp)
	if m["isError"] != true {
		t.Fatalf("isError = %v, want true", m["isError"])
	}
	var structured relay.Error
	if err := json.Unmarshal([]byte(contentText(t, resp)), &structured); err != nil {
		t.Fatalf("tool error is not a relay.Error: %v", err)
	}
	if structured.Code != relay.CodeNetworkError || !structured.Retryable {
		t.Fatalf("structured error = %#v, want retryable NETWORK_ERROR", structured)
	}
}

func TestToolsCallMissingParams(t *testing.T) {
	s := newServer(&stubSource{tools: githubDescriptors()}, &stubInvoker{})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{}}`,
	)

	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}
	for _, resp := range responses {
		if resp.Error == nil || resp.Error.Code != CodeInvalidParams {
			t.Fatalf("error = %#v, want %d", resp.Error, CodeInvalidParams)
		}
	}
}

// --- protocol-level errors ----------------------------------------------

func TestUnknownMethod(t *testing.T) {
	s := newServer(&stubSource{}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"2.0","id":10,"method":"prompts/list"}`)

	resp := requireOne(t, responses)
	if resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("error = %#v, want %d", resp.Error, CodeMethodNotFound)
	}
}

func TestInvalidJSONRPCVersion(t *testing.T) {
	s := newServer(&stubSource{}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"1.0","id":11,"method":"ping"}`)

	resp := requireOne(t, responses)
	if resp.Error == nil || resp.Error.Code != CodeInvalidRequest {
		t.Fatalf("error = %#v, want %d", resp.Error, CodeInvalidRequest)
	}
}

func TestParseErrorHasNullID(t *testing.T) {
	s := newServer(&stubSource{}, &stubInvoker{})
	raw, responses := serve(t, s, `{"jsonrpc":"2.0","id":12,"method":`)

	resp := requireOne(t, responses)
	if resp.Error == nil || resp.Error.Code != CodeParseError {
		t.Fatalf("error = %#v, want %d", resp.Error, CodeParseError)
	}
	if !strings.Contains(raw, "\"id\":null") {
		t.Fatalf("parse error should carry a null id, got %s", raw)
	}
}

func TestPing(t *testing.T) {
	s := newServer(&stubSource{}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"2.0","id":13,"method":"ping"}`)

	resp := requireOne(t, responses)
	m := resultMap(t, resp)
	if len(m) != 0 {
		t.Fatalf("ping result = %#v, want empty object", m)
	}
}

// TestDiagnosticsNeverReachStdout proves the §10 discipline: a request
// mistakenly sent as a notification is logged, not answered, and the only
// stdout line is the real response.
func TestDiagnosticsNeverReachStdout(t *testing.T) {
	var log bytes.Buffer
	s := newServer(&stubSource{tools: githubDescriptors()}, &stubInvoker{})
	s.Log = &log

	raw, responses := serve(t, s,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":14,"method":"ping"}`,
	)

	resp := requireOne(t, responses)
	if resp.ID != float64(14) {
		t.Fatalf("response id = %v, want 14", resp.ID)
	}
	if !strings.Contains(log.String(), "ignored request without id") {
		t.Fatalf("expected a diagnostic in the log, got %q", log.String())
	}
	if strings.Contains(raw, "ignored") {
		t.Fatalf("diagnostic leaked to stdout: %s", raw)
	}
}

// --- parity --------------------------------------------------------------

// TestCLIMCPParity is the M6 acceptance check (spec §29, §60): the MCP tool
// definition is a projection of the manifest the CLI derives its verbs and
// flags from, so the two cannot disagree.
func TestCLIMCPParity(t *testing.T) {
	doc := &manifest.Document{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTool,
		Metadata:   manifest.Metadata{Name: "github", Version: "1.0.0"},
		Runtime:    manifest.Runtime{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
		Tools: []manifest.Tool{
			{
				Name:        "get_repository",
				Description: "Get repository information",
				Input: manifest.InputSchema{
					Type: "object",
					Properties: map[string]manifest.Property{
						"owner": {Type: "string"},
						"repo":  {Type: "string"},
					},
					Required: []string{"owner", "repo"},
				},
			},
			{
				Name:        "list_pull_requests",
				Description: "List pull requests",
				Input: manifest.InputSchema{
					Type: "object",
					Properties: map[string]manifest.Property{
						"owner": {Type: "string"},
						"repo":  {Type: "string"},
						"state": {Type: "string"},
					},
				},
			},
		},
	}

	got := descriptorsFor("github", doc)
	if len(got) != len(doc.Tools) {
		t.Fatalf("got %d descriptors, want %d operations", len(got), len(doc.Tools))
	}
	if got[0].MCPName != "github_get_repository" {
		t.Fatalf("MCP name = %q, want github_get_repository (§28)", got[0].MCPName)
	}
	for i, operation := range doc.Tools {
		descriptor := got[i]
		if descriptor.MCPName != "github_"+operation.Name {
			t.Errorf("MCP name %q != github_%s", descriptor.MCPName, operation.Name)
		}
		if descriptor.ToolName != "github" || descriptor.OperationName != operation.Name {
			t.Errorf("descriptor %#v does not target github/%s", descriptor, operation.Name)
		}
		if descriptor.Description != operation.Description {
			t.Errorf("description %q != %q", descriptor.Description, operation.Description)
		}
		encoded, err := json.Marshal(operation.Input)
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(encoded, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(descriptor.InputSchema, want) {
			t.Errorf("input schema drift: got %#v, want %#v", descriptor.InputSchema, want)
		}
	}
}

// --- registry source -----------------------------------------------------

// writeFakeTool writes a shell script that prints the given manifest when run,
// so ExecManifestReader can be exercised against a real process.
func writeFakeTool(t *testing.T, dir, name, manifestJSON string) string {
	t.Helper()
	manifestPath := filepath.Join(dir, name+".manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, name)
	body := "#!/bin/sh\ncat " + manifestPath + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestRegistrySourceReadsBinaryManifest(t *testing.T) {
	dir := t.TempDir()
	doc := &manifest.Document{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTool,
		Metadata:   manifest.Metadata{Name: "github", Version: "1.0.0"},
		Runtime:    manifest.Runtime{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
		Protocol:   manifest.Protocol{Type: "rest"},
		Tools: []manifest.Tool{{
			Name:        "get_repository",
			Description: "Get repository information",
			Input: manifest.InputSchema{
				Type:       "object",
				Properties: map[string]manifest.Property{"owner": {Type: "string"}, "repo": {Type: "string"}},
				Required:   []string{"owner", "repo"},
			},
		}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	script := writeFakeTool(t, dir, "github", string(raw))

	store := registry.New(filepath.Join(dir, "registry"))
	if err := store.Put(relay.Installation{Name: "github", Version: "1.0.0", Path: script, Runtime: relay.RuntimeName}); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	source := &RegistrySource{Registry: store, Read: ExecManifestReader, Log: &log}
	tools, err := source.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	if tools[0].MCPName != "github_get_repository" || tools[0].ToolName != "github" {
		t.Fatalf("descriptor = %#v", tools[0])
	}
	required, _ := tools[0].InputSchema["required"].([]any)
	if len(required) != 2 {
		t.Fatalf("schema required = %#v", tools[0].InputSchema["required"])
	}
}

func TestRegistrySourceSkipsBrokenTool(t *testing.T) {
	dir := t.TempDir()
	doc := &manifest.Document{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTool,
		Metadata:   manifest.Metadata{Name: "github", Version: "1.0.0"},
		Runtime:    manifest.Runtime{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
		Protocol:   manifest.Protocol{Type: "rest"},
		Tools:      []manifest.Tool{{Name: "get_repository", Description: "Get a repo"}},
	}
	raw, _ := json.Marshal(doc)
	script := writeFakeTool(t, dir, "github", string(raw))

	store := registry.New(filepath.Join(dir, "registry"))
	if err := store.Put(relay.Installation{Name: "github", Version: "1.0.0", Path: script, Runtime: relay.RuntimeName}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(relay.Installation{Name: "broken", Version: "1.0.0", Path: filepath.Join(dir, "missing"), Runtime: relay.RuntimeName}); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	source := &RegistrySource{Registry: store, Read: ExecManifestReader, Log: &log}
	tools, err := source.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].ToolName != "github" {
		t.Fatalf("got %#v, want only github", tools)
	}
	if !strings.Contains(log.String(), "broken") {
		t.Fatalf("expected a diagnostic about the broken tool, got %q", log.String())
	}
}

// --- daemon invoker ------------------------------------------------------

func TestDaemonInvokerSuccess(t *testing.T) {
	ops := &fakeOps{resp: relay.InvokeResponse{Success: true, Result: map[string]any{"ok": true}}}
	result, appErr := DaemonInvoker{Operations: ops}.Invoke(context.Background(), "github", "get_repository",
		map[string]any{"owner": "openai"})
	if appErr != nil {
		t.Fatalf("unexpected error: %v", appErr)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if ops.req.Type != relay.FrameInvoke || ops.req.Tool != "github" || ops.req.Operation != "get_repository" {
		t.Fatalf("request = %#v", ops.req)
	}
}

func TestDaemonInvokerPreservesStructuredError(t *testing.T) {
	ops := &fakeOps{err: relay.NewError(relay.CodeTimeout, "daemon timed out")}
	_, appErr := DaemonInvoker{Operations: ops}.Invoke(context.Background(), "github", "op", nil)
	if appErr == nil || appErr.Code != relay.CodeTimeout {
		t.Fatalf("error = %#v, want TIMEOUT", appErr)
	}
}

func TestDaemonInvokerMapsTransportErrorToNetworkError(t *testing.T) {
	ops := &fakeOps{err: errors.New("connection reset")}
	_, appErr := DaemonInvoker{Operations: ops}.Invoke(context.Background(), "github", "op", nil)
	if appErr == nil || appErr.Code != relay.CodeNetworkError {
		t.Fatalf("error = %#v, want NETWORK_ERROR", appErr)
	}
}

func TestDaemonInvokerPreservesResponseError(t *testing.T) {
	ops := &fakeOps{resp: relay.InvokeResponse{Success: false, Error: relay.NewError(relay.CodeAuthRequired, "credential missing")}}
	_, appErr := DaemonInvoker{Operations: ops}.Invoke(context.Background(), "github", "op", nil)
	if appErr == nil || appErr.Code != relay.CodeAuthRequired {
		t.Fatalf("error = %#v, want AUTH_REQUIRED", appErr)
	}
}

func TestDaemonInvokerWithoutIPCIsNetworkError(t *testing.T) {
	_, appErr := DaemonInvoker{}.Invoke(context.Background(), "github", "op", nil)
	if appErr == nil || appErr.Code != relay.CodeNetworkError {
		t.Fatalf("error = %#v, want NETWORK_ERROR", appErr)
	}
}

func TestDaemonInvokerNormalizesNilInput(t *testing.T) {
	ops := &fakeOps{resp: relay.InvokeResponse{Success: true, Result: "ok"}}
	if _, appErr := (DaemonInvoker{Operations: ops}).Invoke(context.Background(), "github", "op", nil); appErr != nil {
		t.Fatal(appErr)
	}
	if ops.req.Input == nil {
		t.Fatal("nil input should be sent as an empty object")
	}
}
