package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"relay/internal/ipc"
	"relay/internal/paths"
	"relay/internal/permissions"
	"relay/internal/protocol"
	"relay/internal/registry"
	"relay/internal/telemetry"
	"relay/pkg/relay"
)

// destructiveManifest declares two operations the default policy gates plus an
// ordinary one, so the tests can tell a gated operation from an unaffected one
// with a single registered tool (spec §25).
const destructiveManifest = `apiVersion: relay/v1
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
  - name: delete_repository
    description: delete a repository
    input:
      type: object
    request:
      method: DELETE
      path: /repos
  - name: charge_customer
    description: charge a customer
    input:
      type: object
    request:
      method: POST
      path: /charges
  - name: list_repositories
    description: list repositories
    input:
      type: object
    request:
      method: GET
      path: /repos
`

// confirmHarness is a throwaway Relay home with the destructive tool registered
// and a fake executor, so a test can assert both the daemon's decision and
// whether the operation actually reached execution.
type confirmHarness struct {
	daemon   *Daemon
	executor *fakeExecutor
}

func newConfirmHarness(t *testing.T, prep func(layout paths.Layout), usage UsageRecorder) confirmHarness {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	if prep != nil {
		prep(layout)
	}
	if err := registry.New(layout.Registry).Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    writeToolScript(t, destructiveManifest),
		Runtime: "relay/v1",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: map[string]any{"ok": true}}}
	return confirmHarness{
		executor: executor,
		daemon: New(Config{
			Layout:    layout,
			Version:   "test",
			Executors: map[string]protocol.Executor{"rest": executor},
			Keychain:  newMemKeychain(),
			Telemetry: usage,
			Log:       io.Discard,
		}),
	}
}

// invokeOp runs one operation with an explicit acknowledgement.
func invokeOp(d *Daemon, operation, confirmation string) relay.InvokeResponse {
	return d.invokeWith(context.Background(), relay.InvokeRequest{
		Type:      relay.FrameInvoke,
		Tool:      "demo",
		Operation: operation,
		Input:     map[string]any{},
	}, confirmation)
}

// TestDestructiveOperationRequiresConfirmation covers spec §25: an unconfirmed
// destructive operation is refused with the distinct gate, naming the tool and
// the operation, and never reaches the executor.
func TestDestructiveOperationRequiresConfirmation(t *testing.T) {
	harness := newConfirmHarness(t, nil, nil)

	response := invokeOp(harness.daemon, "delete_repository", "")
	if response.Success || response.Error == nil {
		t.Fatal("an unconfirmed destructive operation ran")
	}
	if response.Error.Code != permissions.CodeConfirmationRequired {
		t.Fatalf("code = %s, want %s", response.Error.Code, permissions.CodeConfirmationRequired)
	}
	if response.Error.Details["tool"] != "demo" || response.Error.Details["operation"] != "delete_repository" {
		t.Fatalf("gate did not name the tool and operation: %v", response.Error.Details)
	}
	if len(harness.executor.requests) != 0 {
		t.Fatal("the executor ran a destructive operation that was not confirmed")
	}
}

// TestDestructiveOperationRunsOnceConfirmed covers spec §25: the matching
// acknowledgement lets the very same operation through.
func TestDestructiveOperationRunsOnceConfirmed(t *testing.T) {
	harness := newConfirmHarness(t, nil, nil)

	response := invokeOp(harness.daemon, "delete_repository", ipc.ConfirmationToken("demo", "delete_repository"))
	if !response.Success {
		t.Fatalf("confirmed destructive operation failed: %v", response.Error)
	}
	if len(harness.executor.requests) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(harness.executor.requests))
	}
}

// TestConfirmationDoesNotTransferBetweenOperations covers spec §25: the
// acknowledgement is bound to one operation, so it cannot unlock another.
func TestConfirmationDoesNotTransferBetweenOperations(t *testing.T) {
	harness := newConfirmHarness(t, nil, nil)

	response := invokeOp(harness.daemon, "delete_repository", ipc.ConfirmationToken("demo", "charge_customer"))
	if response.Success || response.Error == nil || response.Error.Code != permissions.CodeConfirmationRequired {
		t.Fatalf("confirmation for another operation was accepted: %+v", response.Error)
	}
	if len(harness.executor.requests) != 0 {
		t.Fatal("a mis-bound confirmation let the executor run")
	}
}

// TestUnlistedOperationIsUnaffected covers spec §25: a non-destructive
// operation runs without any acknowledgement.
func TestUnlistedOperationIsUnaffected(t *testing.T) {
	harness := newConfirmHarness(t, nil, nil)

	response := invokeOp(harness.daemon, "list_repositories", "")
	if !response.Success {
		t.Fatalf("an unlisted operation was gated: %v", response.Error)
	}
	if len(harness.executor.requests) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(harness.executor.requests))
	}
}

// TestMCPPlainRequestStillDenied covers spec §41: the MCP adapter sends a plain
// relay.InvokeRequest with no confirmation field, so it keeps receiving the
// denial. There is no MCP-only permission path to bypass the gate.
func TestMCPPlainRequestStillDenied(t *testing.T) {
	harness := newConfirmHarness(t, nil, nil)

	// Exactly the frame internal/mcp/source.go sends: a bare InvokeRequest.
	frame, err := json.Marshal(relay.InvokeRequest{
		Type:      relay.FrameInvoke,
		Tool:      "demo",
		Operation: "delete_repository",
		Input:     map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal MCP frame: %v", err)
	}
	if strings.Contains(string(frame), ipc.ConfirmationField) {
		t.Fatalf("the MCP frame unexpectedly carries a confirmation: %s", frame)
	}

	reply, err := harness.daemon.Handle(context.Background(), relay.FrameInvoke, frame)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	response, ok := reply.(relay.InvokeResponse)
	if !ok {
		t.Fatalf("reply type = %T, want relay.InvokeResponse", reply)
	}
	if response.Success || response.Error == nil || response.Error.Code != permissions.CodeConfirmationRequired {
		t.Fatalf("MCP-style frame was not denied with the gate: %+v", response.Error)
	}
	if len(harness.executor.requests) != 0 {
		t.Fatal("an MCP-style frame ran a destructive operation")
	}
}

// TestDefaultPolicyOnWhenFileMissing covers the backward-compatibility
// requirement: with no permissions.yaml the daemon still enforces the default
// destructive list, so installing this feature cannot silently weaken behavior.
func TestDefaultPolicyOnWhenFileMissing(t *testing.T) {
	harness := newConfirmHarness(t, nil, nil)
	if !slices.Equal(harness.daemon.destructiveOps, DefaultDestructiveOps) {
		t.Fatalf("destructive ops = %v, want the default %v", harness.daemon.destructiveOps, DefaultDestructiveOps)
	}
	for _, operation := range DefaultDestructiveOps {
		if response := invokeOp(harness.daemon, operation, ""); response.Success || response.Error == nil {
			t.Fatalf("default-gated operation %q ran unconfirmed", operation)
		}
	}
}

// TestPermissionsFileDrivesTheList covers spec §25's configurability: a declared
// list replaces the default with no code change, so an operation the default
// gated now runs and one the file names is gated.
func TestPermissionsFileDrivesTheList(t *testing.T) {
	harness := newConfirmHarness(t, func(layout paths.Layout) {
		writePermissionsFile(t, layout.Config, "destructive:\n  - list_repositories\n")
	}, nil)

	if !slices.Equal(harness.daemon.destructiveOps, []string{"list_repositories"}) {
		t.Fatalf("destructive ops = %v, want the declared list", harness.daemon.destructiveOps)
	}
	// delete_repository is no longer listed, so it runs; list_repositories now is.
	if response := invokeOp(harness.daemon, "delete_repository", ""); !response.Success {
		t.Fatalf("a delisted operation was still gated: %v", response.Error)
	}
	if response := invokeOp(harness.daemon, "list_repositories", ""); response.Error == nil || response.Error.Code != permissions.CodeConfirmationRequired {
		t.Fatalf("a newly listed operation was not gated: %+v", response)
	}
}

// TestMalformedPermissionsFileRefusesToServe covers the hard-error convention:
// a malformed policy leaves the daemon fail-closed and Run refuses to start.
func TestMalformedPermissionsFileRefusesToServe(t *testing.T) {
	harness := newConfirmHarness(t, func(layout paths.Layout) {
		writePermissionsFile(t, layout.Config, "destructive: [oops\n")
	}, nil)

	if harness.daemon.configErr == nil {
		t.Fatal("malformed policy did not surface as a config error")
	}
	if !slices.Equal(harness.daemon.destructiveOps, DefaultDestructiveOps) {
		t.Fatalf("malformed policy did not fall back to the default: %v", harness.daemon.destructiveOps)
	}
	if err := harness.daemon.Run(context.Background(), nil); err == nil {
		t.Fatal("Run started despite a malformed permissions policy")
	}
}

// TestConfirmedInvocationIsRecordedWithoutTheToken covers spec §31: a confirmed
// invocation is recorded like any other, and the acknowledgement itself is not a
// secret and is never persisted.
func TestConfirmedInvocationIsRecordedWithoutTheToken(t *testing.T) {
	buffer := &bytes.Buffer{}
	harness := newConfirmHarness(t, nil, telemetry.NewWriter(buffer))

	response := invokeOp(harness.daemon, "delete_repository", ipc.ConfirmationToken("demo", "delete_repository"))
	if !response.Success {
		t.Fatalf("confirmed destructive operation failed: %v", response.Error)
	}
	events := recordedEvents(t, buffer)
	if len(events) != 1 || !events[0].Success || events[0].Operation != "delete_repository" {
		t.Fatalf("confirmed invocation was not recorded plainly: %+v", events)
	}
	if strings.Contains(buffer.String(), "confirm:") {
		t.Fatalf("the confirmation token leaked into telemetry: %s", buffer.String())
	}
}
