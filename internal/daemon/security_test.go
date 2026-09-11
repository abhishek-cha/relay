package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// This file is the M8 daemon half of the security suite (spec §40, §59). It
// exercises the invariants "the daemon is the trusted component" and "tool
// binaries are untrusted" at the seams most likely to break:
//
//   - a credential that the daemon injects must never appear in an invoke
//     reply, in a daemon diagnostic, or anywhere under the Relay home
//     (spec §21, §22, §54);
//   - a rejected credential (401) must map to AUTH_FAILED without echoing the
//     secret back (spec §26);
//   - an unregistered tool or operation never reaches an executor;
//   - the daemon re-validates input itself, because a tool binary may skip its
//     own CLI validation (spec §40);
//   - a descriptor declaring an unsupported runtime apiVersion is refused with
//     RUNTIME_INCOMPATIBLE rather than executed (spec §35).
//
// Every test builds the daemon over a throwaway RELAY_HOME with an in-memory
// Keychain and a fake executor, so nothing here touches the real macOS
// Keychain, the real ~/.relay, or the network.

// secDaemonSentinel is a distinctive value the tests grep output for. If it ever
// shows up outside a keychain read, a credential leaked.
const secDaemonSentinel = "RELAY-SENTINEL-SECRET-da1mon-4f8c-DO-NOT-LEAK"

// securityInputManifest declares one operation with a required input property,
// so the daemon's re-validation has something to reject.
const securityInputManifest = `apiVersion: relay/v1
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
      properties:
        owner:
          type: string
      required:
        - owner
    request:
      method: GET
      path: /ping
`

// securityRuntimeV2Manifest declares a runtime major this daemon does not
// provide, so the compatibility gate must fire before any execution.
const securityRuntimeV2Manifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
runtime:
  name: relay
  apiVersion: v2
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

// secDaemonFaultyKeychain returns the secret it holds alongside an error that
// embeds it, standing in for a Store whose failure text is unsafe. The daemon
// must scrub the value before it builds AUTH_FAILED (spec §22).
type secDaemonFaultyKeychain struct{ secret string }

func (k *secDaemonFaultyKeychain) Get(string, string) (string, error) {
	return k.secret, fmt.Errorf("vault exploded while holding %s", k.secret)
}

func (k *secDaemonFaultyKeychain) Set(string, string, string) error { return nil }
func (k *secDaemonFaultyKeychain) Delete(string, string) error      { return nil }

// newSecDaemonHarness is a log-capturing variant of newDaemonHarness: the same
// throwaway home, seeded registry, and injected Keychain, plus a caller-owned
// diagnostics sink so a test can assert nothing sensitive was logged.
func newSecDaemonHarness(t *testing.T, manifestYAML string, executor protocol.Executor, kc keychain.Store, logw io.Writer) *Daemon {
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
		Log:       logw,
	})
}

// secMarshalJSON renders a reply the way the IPC layer would put it on the wire,
// so a test greps the exact bytes a caller would observe.
func secMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	return string(encoded)
}

// secAssertSecretAbsentFromTree asserts the sentinel appears in no file under
// root. This is the strongest form of "no observable output": the secret must
// not be persisted into the registry, logs, cache, or anything else the daemon
// owns (spec §22, §54).
func secAssertSecretAbsentFromTree(t *testing.T, root, secret string) {
	t.Helper()
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), secret) {
			t.Errorf("credential leaked into %s", path)
		}
		return nil
	})
}

// writeSecurityDescriptorScript writes an executable that serves a fixed
// --describe reply, standing in for a tool binary the daemon is asked to install.
func writeSecurityDescriptorScript(t *testing.T, descriptorJSON string) string {
	t.Helper()
	dir := t.TempDir()
	payload := filepath.Join(dir, "descriptor.json")
	if err := os.WriteFile(payload, []byte(descriptorJSON), 0o644); err != nil {
		t.Fatalf("write descriptor: %v", err)
	}
	script := filepath.Join(dir, "demo")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--describe\" ]; then\n" +
		"cat " + payload + "\n" +
		"fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write descriptor script: %v", err)
	}
	return script
}

// TestSecurityDaemonCredentialNeverLeaksOnSuccess: after an authenticated invoke
// the injected secret is in the executor's hands but in no observable output —
// not the reply, not the daemon log, not any file under the Relay home.
func TestSecurityDaemonCredentialNeverLeaksOnSuccess(t *testing.T) {
	kc := newMemKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, secDaemonSentinel); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: map[string]any{"ok": true}}}
	var log bytes.Buffer
	d := newSecDaemonHarness(t, authManifest, executor, kc, &log)

	response := invoke(t, d)
	if !response.Success {
		t.Fatalf("invoke failed: %v", response.Error)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(executor.requests))
	}
	credential := executor.requests[0].Credential
	if credential == nil || credential.Secret != secDaemonSentinel {
		t.Fatalf("credential was not injected, so the test would prove nothing: %+v", credential)
	}

	if raw := secMarshalJSON(t, response); strings.Contains(raw, secDaemonSentinel) {
		t.Fatalf("invoke reply leaked the credential: %s", raw)
	}
	if strings.Contains(log.String(), secDaemonSentinel) {
		t.Fatalf("daemon log leaked the credential: %s", log.String())
	}
	secAssertSecretAbsentFromTree(t, paths.Default().Root, secDaemonSentinel)
}

// TestSecurityDaemonNoLeakOnRejectedCredential: a service that refuses the
// stored credential becomes AUTH_FAILED, and neither the error payload nor the
// log carries the secret (spec §26).
func TestSecurityDaemonNoLeakOnRejectedCredential(t *testing.T) {
	kc := newMemKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, secDaemonSentinel); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	// The REST executor maps 401 to AUTH_REQUIRED carrying only transport
	// details; the daemon refines it to AUTH_FAILED because a credential was
	// actually injected.
	executor := &fakeExecutor{err: relay.NewError(relay.CodeAuthRequired, "authentication required").
		WithDetails(map[string]any{"httpStatus": 401, "method": "GET", "url": "https://example.test/ping"})}
	var log bytes.Buffer
	d := newSecDaemonHarness(t, authManifest, executor, kc, &log)

	response := invoke(t, d)
	if response.Success || response.Error == nil {
		t.Fatal("invoke succeeded, want AUTH_FAILED")
	}
	if response.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeAuthFailed)
	}
	if raw := secMarshalJSON(t, response); strings.Contains(raw, secDaemonSentinel) {
		t.Fatalf("AUTH_FAILED error payload echoed the credential: %s", raw)
	}
	if strings.Contains(log.String(), secDaemonSentinel) {
		t.Fatalf("daemon log leaked the credential: %s", log.String())
	}
	secAssertSecretAbsentFromTree(t, paths.Default().Root, secDaemonSentinel)
}

// TestSecurityDaemonAuthFailedScrubsKeychainError: even a Store whose failure
// text embeds the secret must not surface it through AUTH_FAILED.
func TestSecurityDaemonAuthFailedScrubsKeychainError(t *testing.T) {
	d := newSecDaemonHarness(t, authManifest, &fakeExecutor{}, &secDaemonFaultyKeychain{secret: secDaemonSentinel}, io.Discard)

	response := invoke(t, d)
	if response.Success || response.Error == nil {
		t.Fatal("invoke succeeded, want AUTH_FAILED from the failed keychain read")
	}
	if response.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeAuthFailed)
	}
	if raw := secMarshalJSON(t, response); strings.Contains(raw, secDaemonSentinel) {
		t.Fatalf("AUTH_FAILED carried the credential: %s", raw)
	}
}

// TestSecurityDaemonUnregisteredToolNeverReachesExecutor: a tool missing from
// the registry, and an operation missing from a registered tool's manifest, are
// both refused before any executor runs.
func TestSecurityDaemonUnregisteredToolNeverReachesExecutor(t *testing.T) {
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: "ok"}}
	d := newSecDaemonHarness(t, noAuthManifest, executor, newMemKeychain(), io.Discard)
	ctx := context.Background()

	response := d.invoke(ctx, relay.InvokeRequest{Tool: "ghost", Operation: "ping", Input: map[string]any{}})
	if response.Success || response.Error == nil {
		t.Fatal("invoking an unregistered tool succeeded")
	}
	if response.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeToolNotFound)
	}

	response = d.invoke(ctx, relay.InvokeRequest{Tool: "demo", Operation: "nope", Input: map[string]any{}})
	if response.Success || response.Error == nil {
		t.Fatal("invoking an undeclared operation succeeded")
	}
	if response.Error.Code != relay.CodeOperationNotFound {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeOperationNotFound)
	}

	if len(executor.requests) != 0 {
		t.Fatalf("executor ran %d times for tools it should never have seen, want 0", len(executor.requests))
	}
}

// TestSecurityDaemonRevalidatesUntrustedInput: the daemon validates input
// against the manifest itself, so a tool binary that skipped its own CLI
// validation cannot smuggle an invalid invocation past it (spec §40).
func TestSecurityDaemonRevalidatesUntrustedInput(t *testing.T) {
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: "ok"}}
	d := newSecDaemonHarness(t, securityInputManifest, executor, newMemKeychain(), io.Discard)
	ctx := context.Background()

	cases := []struct {
		name  string
		input map[string]any
	}{
		{"missing required property", map[string]any{}},
		{"unknown property", map[string]any{"owner": "openai", "evil": "x"}},
		{"wrong type", map[string]any{"owner": 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := d.invoke(ctx, relay.InvokeRequest{Tool: "demo", Operation: "ping", Input: tc.input})
			if response.Success || response.Error == nil {
				t.Fatalf("invalid input passed re-validation: %+v", response)
			}
			if response.Error.Code != relay.CodeInvalidInput {
				t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeInvalidInput)
			}
		})
	}
	if len(executor.requests) != 0 {
		t.Fatalf("executor ran %d times for invalid input, want 0", len(executor.requests))
	}

	// Rejection is per-invocation: a well-formed call still runs afterwards,
	// which proves the gate validates rather than being sticky.
	response := d.invoke(ctx, relay.InvokeRequest{Tool: "demo", Operation: "ping", Input: map[string]any{"owner": "openai"}})
	if !response.Success {
		t.Fatalf("valid input was rejected: %v", response.Error)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("executor ran %d times for valid input, want 1", len(executor.requests))
	}
}

// TestSecurityDaemonRuntimeIncompatibleManifestNeverExecutes: a tool whose
// manifest declares an unsupported runtime major is refused with
// RUNTIME_INCOMPATIBLE and never handed to an executor (spec §35).
func TestSecurityDaemonRuntimeIncompatibleManifestNeverExecutes(t *testing.T) {
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: "ok"}}
	d := newSecDaemonHarness(t, securityRuntimeV2Manifest, executor, newMemKeychain(), io.Discard)

	response := d.invoke(context.Background(), relay.InvokeRequest{Tool: "demo", Operation: "ping", Input: map[string]any{}})
	if response.Success || response.Error == nil {
		t.Fatalf("a runtime-incompatible tool was executed: %+v", response)
	}
	if response.Error.Code != relay.CodeRuntimeIncompatible {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeRuntimeIncompatible)
	}
	if len(executor.requests) != 0 {
		t.Fatal("executor ran for a runtime-incompatible tool")
	}
}

// TestSecurityDaemonRuntimeIncompatibleDescriptorRefused: a descriptor that
// declares an unsupported runtime apiVersion is refused at install time and
// leaves nothing registered. This is the --describe contract a tool binary
// speaks, so the daemon must validate it rather than trust the caller
// (spec §9, §35, §40).
func TestSecurityDaemonRuntimeIncompatibleDescriptorRefused(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	d := New(Config{
		Layout:    layout,
		Version:   "test",
		Executors: map[string]protocol.Executor{"rest": &fakeExecutor{}},
		Keychain:  newMemKeychain(),
		Log:       io.Discard,
	})

	descriptor := `{"apiVersion":"relay/v1","kind":"Tool","name":"demo","version":"1.0.0","protocol":"rest","runtime":{"name":"relay","apiVersion":"v9"},"tools":[{"name":"ping"}]}`
	path := writeSecurityDescriptorScript(t, descriptor)

	response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: path})
	if response.Success || response.Error == nil {
		t.Fatalf("a runtime-incompatible descriptor was accepted: %+v", response)
	}
	if response.Error.Code != relay.CodeRuntimeIncompatible {
		t.Fatalf("code = %s, want %s", response.Error.Code, relay.CodeRuntimeIncompatible)
	}
	if _, err := registry.New(layout.Registry).Get("demo"); err == nil {
		t.Fatal("an incompatible descriptor left a registry record behind")
	}
}
