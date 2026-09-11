package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relay/internal/keychain"
	"relay/internal/paths"
	"relay/internal/protocol"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// authManifest declares bearer auth; noAuthManifest declares none. They are the
// two shapes the daemon has to treat differently (spec §21).
const authManifest = `apiVersion: relay/v1
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

const noAuthManifest = `apiVersion: relay/v1
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
tools:
  - name: ping
    description: ping the example service
    input:
      type: object
    request:
      method: GET
      path: /ping
`

// memKeychain is an in-memory keychain.Store so the daemon can be built without
// the real macOS Keychain (spec §40).
type memKeychain struct{ values map[string]string }

func newMemKeychain() *memKeychain { return &memKeychain{values: map[string]string{}} }

func (m *memKeychain) key(service, account string) string { return service + "|" + account }

func (m *memKeychain) Get(service, account string) (string, error) {
	if value, ok := m.values[m.key(service, account)]; ok {
		return value, nil
	}
	return "", keychain.ErrNotFound
}

func (m *memKeychain) Set(service, account, secret string) error {
	m.values[m.key(service, account)] = secret
	return nil
}

func (m *memKeychain) Delete(service, account string) error {
	delete(m.values, m.key(service, account))
	return nil
}

// fakeExecutor records what the daemon asked it to run and replies with a canned
// result, so tests can assert on the injected credential and the error mapping.
type fakeExecutor struct {
	requests []protocol.Request
	response protocol.Response
	err      error
}

func (f *fakeExecutor) Execute(_ context.Context, request protocol.Request) (protocol.Response, error) {
	f.requests = append(f.requests, request)
	return f.response, f.err
}

// writeToolScript writes an executable that serves a fixed manifest for
// --manifest, standing in for an installed Relay tool.
func writeToolScript(t *testing.T, manifestYAML string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--manifest\" ]; then\n" +
		"cat <<'RELAY_HEREDOC'\n" +
		manifestYAML +
		"RELAY_HEREDOC\n" +
		"fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	return path
}

// newDaemonHarness builds a daemon over a throwaway Relay home with one
// registered tool and an injected fake Keychain.
func newDaemonHarness(t *testing.T, manifestYAML string, executor protocol.Executor, kc keychain.Store) *Daemon {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	if err := registry.New(layout.Registry).Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    writeToolScript(t, manifestYAML),
		Runtime: "relay/v1",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	return New(Config{
		Layout:    layout,
		Version:   "test",
		Executors: map[string]protocol.Executor{"rest": executor},
		Keychain:  kc,
		Log:       io.Discard,
	})
}

func invoke(t *testing.T, d *Daemon) relay.InvokeResponse {
	t.Helper()
	return d.invoke(context.Background(), relay.InvokeRequest{
		Type:      relay.FrameInvoke,
		Tool:      "demo",
		Operation: "ping",
		Input:     map[string]any{},
	})
}

func TestInvokeWithoutAuthIsUnaffected(t *testing.T) {
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: map[string]any{"ok": true}}}
	d := newDaemonHarness(t, noAuthManifest, executor, newMemKeychain())

	response := invoke(t, d)
	if !response.Success {
		t.Fatalf("invoke failed: %v", response.Error)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(executor.requests))
	}
	if executor.requests[0].Credential != nil {
		t.Fatal("a tool with no auth was handed a credential")
	}
}

func TestInvokeMissingCredentialIsAuthRequired(t *testing.T) {
	executor := &fakeExecutor{response: protocol.Response{Status: 200}}
	d := newDaemonHarness(t, authManifest, executor, newMemKeychain())

	response := invoke(t, d)
	if response.Success || response.Error == nil {
		t.Fatal("invoke succeeded without a stored credential, want AUTH_REQUIRED")
	}
	if response.Error.Code != relay.CodeAuthRequired {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeAuthRequired)
	}
	if len(executor.requests) != 0 {
		t.Fatal("the executor ran even though no credential was available")
	}
}

func TestInvokeInjectsStoredCredential(t *testing.T) {
	kc := newMemKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, "token-abc"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: "ok"}}
	d := newDaemonHarness(t, authManifest, executor, kc)

	response := invoke(t, d)
	if !response.Success {
		t.Fatalf("invoke failed: %v", response.Error)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(executor.requests))
	}
	credential := executor.requests[0].Credential
	if credential == nil {
		t.Fatal("executor got no credential for an auth tool")
	}
	if credential.Header != "Authorization" || credential.Scheme != "Bearer" || credential.Secret != "token-abc" {
		t.Fatalf("injected credential = %+v, want Authorization/Bearer/token-abc", credential)
	}
}

func TestInvokeCredentialRejectedByServiceIsAuthFailed(t *testing.T) {
	kc := newMemKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, "stale-token"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	// The REST executor maps a 401 to AUTH_REQUIRED; the daemon must refine it
	// because a credential was actually injected (spec §26).
	executor := &fakeExecutor{err: relay.NewError(relay.CodeAuthRequired, "authentication required")}
	d := newDaemonHarness(t, authManifest, executor, kc)

	response := invoke(t, d)
	if response.Success || response.Error == nil {
		t.Fatal("invoke succeeded, want AUTH_FAILED")
	}
	if response.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeAuthFailed)
	}
}

func TestAuthStatusLeaksNothing(t *testing.T) {
	const secret = "super-secret-token"
	kc := newMemKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, secret); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, kc)

	frame, err := json.Marshal(relay.AuthStatusRequest{Type: relay.FrameAuthStatus, Tool: "demo"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reply, err := d.Handle(context.Background(), relay.FrameAuthStatus, frame)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	status, ok := reply.(relay.AuthStatusResponse)
	if !ok {
		t.Fatalf("reply type = %T, want relay.AuthStatusResponse", reply)
	}
	if !status.Success || !status.Stored {
		t.Fatalf("status = %+v, want success with a stored credential", status)
	}
	if status.AuthType != "bearer" || status.Provider != "example" {
		t.Fatalf("status declared type/provider = %q/%q, want bearer/example", status.AuthType, status.Provider)
	}
	raw, _ := json.Marshal(reply)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("status reply leaked the secret: %s", raw)
	}
}

func TestAuthSetStoresWithoutEchoing(t *testing.T) {
	const secret = "super-secret-token"
	kc := newMemKeychain()
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, kc)

	frame, err := json.Marshal(relay.AuthSetRequest{Type: relay.FrameAuthSet, Tool: "demo", Secret: secret})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reply, err := d.Handle(context.Background(), relay.FrameAuthSet, frame)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mutation, ok := reply.(relay.MutationResponse)
	if !ok || !mutation.Success {
		t.Fatalf("reply = %+v, want success", reply)
	}
	raw, _ := json.Marshal(reply)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("set reply leaked the secret: %s", raw)
	}
	stored, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault)
	if err != nil || stored != secret {
		t.Fatalf("stored value = %q (%v), want the seeded secret", stored, err)
	}
}

func TestAuthSetRejectsContradictoryType(t *testing.T) {
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, newMemKeychain())

	frame, _ := json.Marshal(relay.AuthSetRequest{
		Type: relay.FrameAuthSet, Tool: "demo", AuthType: "api_key", Secret: "x",
	})
	reply, err := d.Handle(context.Background(), relay.FrameAuthSet, frame)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mutation, ok := reply.(relay.MutationResponse)
	if !ok || mutation.Success || mutation.Error == nil {
		t.Fatalf("reply = %+v, want a failure", reply)
	}
	if mutation.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("code = %s, want %s", mutation.Error.Code, relay.CodeInvalidInput)
	}
}

func TestAuthClearRemovesCredential(t *testing.T) {
	kc := newMemKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, "secret"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, kc)

	frame, _ := json.Marshal(relay.AuthClearRequest{Type: relay.FrameAuthClear, Tool: "demo"})
	reply, err := d.Handle(context.Background(), relay.FrameAuthClear, frame)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	mutation, ok := reply.(relay.MutationResponse)
	if !ok || !mutation.Success {
		t.Fatalf("reply = %+v, want success", reply)
	}
	if _, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault); err == nil {
		t.Fatal("credential still present after auth_clear")
	}
}

func TestAuthUnknownTool(t *testing.T) {
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, newMemKeychain())
	ctx := context.Background()

	setFrame, _ := json.Marshal(relay.AuthSetRequest{Type: relay.FrameAuthSet, Tool: "missing", Secret: "x"})
	setReply, _ := d.Handle(ctx, relay.FrameAuthSet, setFrame)
	if mutation := setReply.(relay.MutationResponse); mutation.Error == nil || mutation.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("auth_set unknown tool = %+v, want TOOL_NOT_FOUND", setReply)
	}

	statusFrame, _ := json.Marshal(relay.AuthStatusRequest{Type: relay.FrameAuthStatus, Tool: "missing"})
	statusReply, _ := d.Handle(ctx, relay.FrameAuthStatus, statusFrame)
	if status := statusReply.(relay.AuthStatusResponse); status.Error == nil || status.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("auth_status unknown tool = %+v, want TOOL_NOT_FOUND", statusReply)
	}
}
