package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relay/internal/daemon"
	"relay/internal/keychain"
	"relay/internal/paths"
	"relay/internal/protocol"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// This file is the M8 MCP half of the security suite (spec §41, §59). It proves
// the three properties that keep MCP from becoming a second security model:
//
//   - MCP inherits the CLI/daemon auth path exactly. The same invoke through the
//     MCP handler and through the daemon's direct handle yields the same
//     structured error code, because MCP has no credential route of its own
//     (spec §41);
//   - malformed tool-call input is rejected with a structured JSON-RPC error
//     instead of panicking or wedging the serve loop (spec §29);
//   - a credential the daemon injects never appears in a JSON-RPC frame
//     (spec §22, §54).
//
// The MCP path is wired to a real daemon over a throwaway RELAY_HOME with an
// in-memory Keychain and a fake executor: no real macOS Keychain, no real
// ~/.relay, no network.

// mcpSecuritySentinel is a distinctive value the tests grep the wire for.
const mcpSecuritySentinel = "RELAY-SENTINEL-SECRET-mcp-7b21-DO-NOT-LEAK"

// secMCPAuthManifest declares bearer auth so credential resolution is exercised
// on the MCP path.
const secMCPAuthManifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: rest
  baseUrl: https://example.test
auth:
  type: bearer
  provider: example
tools:
  - name: ping
    description: ping the example service
    input:
      type: object
    request:
      method: GET
      path: /ping
`

// secMCPKeychain is an in-memory keychain.Store so the daemon behind the MCP
// path never touches the real Keychain.
type secMCPKeychain struct{ values map[string]string }

func newSecMCPKeychain() *secMCPKeychain { return &secMCPKeychain{values: map[string]string{}} }

func (k *secMCPKeychain) id(service, account string) string { return service + "|" + account }

func (k *secMCPKeychain) Get(service, account string) (string, error) {
	if value, ok := k.values[k.id(service, account)]; ok {
		return value, nil
	}
	return "", keychain.ErrNotFound
}

func (k *secMCPKeychain) Set(service, account, secret string) error {
	k.values[k.id(service, account)] = secret
	return nil
}

func (k *secMCPKeychain) Delete(service, account string) error {
	delete(k.values, k.id(service, account))
	return nil
}

// secMCPExecutor records what the daemon asked it to run and replies with a
// canned result, so a test can assert on the injected credential.
type secMCPExecutor struct {
	requests []protocol.Request
	response protocol.Response
	err      error
}

func (e *secMCPExecutor) Execute(_ context.Context, request protocol.Request) (protocol.Response, error) {
	e.requests = append(e.requests, request)
	return e.response, e.err
}

// secDaemonOps adapts *daemon.Daemon to the MCP OperationInvoker seam the same
// way toolruntime.DaemonInvoker does over the socket: it marshals an
// InvokeRequest and hands it to Daemon.Handle. Using the real DaemonInvoker
// above it means the MCP tests exercise the production adapter, not a test-only
// shortcut around it.
type secDaemonOps struct{ d *daemon.Daemon }

func (o secDaemonOps) Invoke(ctx context.Context, request relay.InvokeRequest) (relay.InvokeResponse, error) {
	frame, err := json.Marshal(request)
	if err != nil {
		return relay.InvokeResponse{}, err
	}
	reply, err := o.d.Handle(ctx, relay.FrameInvoke, frame)
	if err != nil {
		return relay.InvokeResponse{}, err
	}
	response, ok := reply.(relay.InvokeResponse)
	if !ok {
		return relay.InvokeResponse{}, fmt.Errorf("daemon returned %T, want relay.InvokeResponse", reply)
	}
	return response, nil
}

// secMCPToolScript writes an executable that serves a fixed manifest for
// --manifest, standing in for an installed Relay tool.
func secMCPToolScript(t *testing.T, manifestYAML string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--manifest\" ]; then\n" +
		"cat <<'RELAY_SECURITY_HEREDOC'\n" +
		manifestYAML +
		"RELAY_SECURITY_HEREDOC\n" +
		"fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	return path
}

// newSecMCPDaemon builds a real daemon over a throwaway RELAY_HOME with one
// registered tool and an injected fake Keychain.
func newSecMCPDaemon(t *testing.T, manifestYAML string, executor protocol.Executor, kc keychain.Store) *daemon.Daemon {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	if err := registry.New(layout.Registry).Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    secMCPToolScript(t, manifestYAML),
		Runtime: "relay/v1",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	return daemon.New(daemon.Config{
		Layout:    layout,
		Version:   "test",
		Executors: map[string]protocol.Executor{"rest": executor},
		Keychain:  kc,
		Log:       io.Discard,
	})
}

// secMCPDescriptors is the minimal catalog that resolves demo_ping to the
// registered demo/ping operation.
func secMCPDescriptors() []ToolDescriptor {
	return []ToolDescriptor{{
		MCPName:       "demo_ping",
		ToolName:      "demo",
		OperationName: "ping",
		Description:   "ping the example service",
		InputSchema:   map[string]any{"type": "object"},
	}}
}

// TestSecurityMCPCLIAuthParity: with no credential configured, the MCP handler
// and the daemon's direct handle return the same structured error code, which
// is what proves MCP has no credential route of its own (spec §41).
func TestSecurityMCPCLIAuthParity(t *testing.T) {
	executor := &secMCPExecutor{response: protocol.Response{Status: 200, Body: "ok"}}
	d := newSecMCPDaemon(t, secMCPAuthManifest, executor, newSecMCPKeychain())

	// Direct (the path the CLI takes): one invoke frame into the daemon.
	frame, err := json.Marshal(relay.InvokeRequest{Type: relay.FrameInvoke, Tool: "demo", Operation: "ping", Input: map[string]any{}})
	if err != nil {
		t.Fatalf("marshal invoke: %v", err)
	}
	directReply, err := d.Handle(context.Background(), relay.FrameInvoke, frame)
	if err != nil {
		t.Fatalf("direct Handle: %v", err)
	}
	direct, ok := directReply.(relay.InvokeResponse)
	if !ok || direct.Error == nil {
		t.Fatalf("direct invoke = %+v, want a structured AUTH_REQUIRED failure", directReply)
	}
	if direct.Error.Code != relay.CodeAuthRequired {
		t.Fatalf("direct code = %s, want %s", direct.Error.Code, relay.CodeAuthRequired)
	}

	// MCP: the real Server plus the real DaemonInvoker, backed by the same daemon.
	s := &Server{
		Source:  &stubSource{tools: secMCPDescriptors()},
		Invoker: DaemonInvoker{Operations: secDaemonOps{d: d}},
		Info:    ServerInfo{Name: "relay-test", Version: "0.0.1"},
		Log:     io.Discard,
	}
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"demo_ping","arguments":{}}}`)
	resp := requireOne(t, responses)
	if resp.Error != nil {
		t.Fatalf("MCP surfaced a protocol fault instead of a tool error: %#v", resp.Error)
	}
	if resultMap(t, resp)["isError"] != true {
		t.Fatalf("MCP tool call was not marked as an error: %#v", resp.Result)
	}
	var structured relay.Error
	if err := json.Unmarshal([]byte(contentText(t, resp)), &structured); err != nil {
		t.Fatalf("MCP tool error is not a relay.Error: %v", err)
	}
	if structured.Code != relay.CodeAuthRequired {
		t.Fatalf("MCP code = %q, want %q", structured.Code, relay.CodeAuthRequired)
	}
	if structured.Code != direct.Error.Code {
		t.Fatalf("MCP and CLI disagree on the auth error: MCP %q, CLI %q", structured.Code, direct.Error.Code)
	}
	if len(executor.requests) != 0 {
		t.Fatal("the executor ran without a credential on the MCP path")
	}
}

// TestSecurityMCPMalformedToolCallIsStructuredError: malformed tool-call input
// is answered with an INVALID_PARAMS frame, and the serve loop keeps running
// afterwards rather than being taken down by one bad request.
func TestSecurityMCPMalformedToolCallIsStructuredError(t *testing.T) {
	s := &Server{
		Source:  &stubSource{tools: secMCPDescriptors()},
		Invoker: &stubInvoker{},
		Info:    ServerInfo{Name: "relay-test", Version: "0.0.1"},
		Log:     io.Discard,
	}

	malformed := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"not-an-object"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":[1,2,3]}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":123}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"demo_ping","arguments":"not-an-object"}}`,
	}
	frames := append(append([]string{}, malformed...), `{"jsonrpc":"2.0","id":99,"method":"ping"}`)
	_, responses := serve(t, s, frames...)
	if len(responses) != len(malformed)+1 {
		t.Fatalf("got %d responses, want %d (one per request)", len(responses), len(malformed)+1)
	}
	for i, resp := range responses[:len(malformed)] {
		if resp.Error == nil || resp.Error.Code != CodeInvalidParams {
			t.Fatalf("malformed case %d = %#v, want JSON-RPC %d", i, resp.Error, CodeInvalidParams)
		}
	}
	last := responses[len(responses)-1]
	if last.Error != nil {
		t.Fatalf("the serve loop did not survive malformed input: %#v", last.Error)
	}
	if m := resultMap(t, last); len(m) != 0 {
		t.Fatalf("expected the loop to still answer ping, got %#v", m)
	}
}

// TestSecurityMCPOversizedFrameDoesNotCrashServer: a frame larger than the
// server's scan cap is refused without panicking and without emitting a partial
// or unattributable frame. An oversize frame is a framing-layer fault (there is
// no decodable id to answer), which is why the server reports a bounded
// bufio.ErrTooLong instead of a JSON-RPC error -- the same policy the daemon's
// IPC reader applies.
func TestSecurityMCPOversizedFrameDoesNotCrashServer(t *testing.T) {
	s := &Server{
		Source:  &stubSource{tools: secMCPDescriptors()},
		Invoker: &stubInvoker{},
		Info:    ServerInfo{Name: "relay-test", Version: "0.0.1"},
		Log:     io.Discard,
	}

	huge := strings.Repeat("a", maxFrameBytes+1024)
	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"demo_ping","arguments":{"blob":"` + huge + `"}}}`
	var out bytes.Buffer
	err := s.Serve(context.Background(), strings.NewReader(frame+"\n"), &out)
	if err == nil {
		t.Fatal("an oversized frame was silently accepted")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("oversized frame error = %v, want bufio.ErrTooLong", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversized frame produced output instead of failing cleanly: %q", out.String())
	}
}

// TestSecurityMCPNeverEmitsCredential: a successful authenticated call injects
// the credential into the executor, and the sentinel appears in no JSON-RPC
// frame the server writes (spec §22, §41, §54).
func TestSecurityMCPNeverEmitsCredential(t *testing.T) {
	kc := newSecMCPKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, mcpSecuritySentinel); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	executor := &secMCPExecutor{response: protocol.Response{Status: 200, Body: map[string]any{"ok": true}}}
	d := newSecMCPDaemon(t, secMCPAuthManifest, executor, kc)

	var log bytes.Buffer
	s := &Server{
		Source:  &stubSource{tools: secMCPDescriptors()},
		Invoker: DaemonInvoker{Operations: secDaemonOps{d: d}},
		Info:    ServerInfo{Name: "relay-test", Version: "0.0.1"},
		Log:     &log,
	}
	raw, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"demo_ping","arguments":{}}}`,
	)

	// The credential really was injected, or the test would prove nothing.
	if len(executor.requests) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(executor.requests))
	}
	credential := executor.requests[0].Credential
	if credential == nil || credential.Secret != mcpSecuritySentinel {
		t.Fatalf("the daemon did not inject the credential: %+v", credential)
	}

	if strings.Contains(raw, mcpSecuritySentinel) {
		t.Fatalf("credential leaked into a JSON-RPC frame: %s", raw)
	}
	if strings.Contains(log.String(), mcpSecuritySentinel) {
		t.Fatalf("credential leaked into the MCP diagnostic log: %s", log.String())
	}

	// Baseline: the call itself succeeded, so the leak assertion is meaningful.
	last := responses[len(responses)-1]
	if m := resultMap(t, last); m["isError"] != false {
		t.Fatalf("tools/call did not succeed: %#v", m)
	}
}
