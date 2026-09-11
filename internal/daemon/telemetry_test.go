package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"relay/internal/paths"
	"relay/internal/protocol"
	"relay/internal/registry"
	"relay/internal/telemetry"
	"relay/pkg/relay"
)

// failingRecorder stands in for a full disk or an unwritable home.
type failingRecorder struct{}

func (failingRecorder) Record(telemetry.Event) error { return errors.New("disk full") }

// newUsageHarness builds a daemon whose usage events land in a buffer, so a test
// can assert on exactly what the daemon recorded (spec §31).
func newUsageHarness(t *testing.T, executor protocol.Executor) (*Daemon, *bytes.Buffer) {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	if err := registry.New(layout.Registry).Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    writeToolScript(t, noAuthManifest),
		Runtime: "relay/v1",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	buffer := &bytes.Buffer{}
	return New(Config{
		Layout:    layout,
		Version:   "test",
		Executors: map[string]protocol.Executor{"rest": executor},
		Keychain:  newMemKeychain(),
		Telemetry: telemetry.NewWriter(buffer),
		Log:       io.Discard,
	}), buffer
}

func recordedEvents(t *testing.T, buffer *bytes.Buffer) []telemetry.Event {
	t.Helper()
	events, err := telemetry.ReadEvents(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatalf("read recorded events: %v", err)
	}
	return events
}

func TestInvokeRecordsOneUsageEvent(t *testing.T) {
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: map[string]any{"ok": true}}}
	daemon, buffer := newUsageHarness(t, executor)

	response := invoke(t, daemon)
	if !response.Success {
		t.Fatalf("invoke failed: %+v", response.Error)
	}

	events := recordedEvents(t, buffer)
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	event := events[0]
	if event.Tool != "demo" || event.Operation != "ping" {
		t.Errorf("event identity = %q/%q, want demo/ping", event.Tool, event.Operation)
	}
	if !event.Success {
		t.Error("event is marked failed after a successful invoke")
	}
	if event.ErrorCode != "" {
		t.Errorf("event carries error code %q on success", event.ErrorCode)
	}
	if event.Timestamp.IsZero() {
		t.Error("event has no timestamp")
	}
	if event.DurationMs < 0 {
		t.Errorf("event has negative duration %d", event.DurationMs)
	}
}

func TestInvokeRecordsFailureWithItsCode(t *testing.T) {
	executor := &fakeExecutor{err: relay.NewError(relay.CodeAuthRequired, "no credential")}
	daemon, buffer := newUsageHarness(t, executor)

	response := invoke(t, daemon)
	if response.Success {
		t.Fatal("invoke unexpectedly succeeded")
	}

	events := recordedEvents(t, buffer)
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Success {
		t.Error("failed invoke was recorded as a success")
	}
	if events[0].ErrorCode != string(relay.CodeAuthRequired) {
		t.Errorf("error code = %q, want %q", events[0].ErrorCode, relay.CodeAuthRequired)
	}
}

func TestInvokeRecordsUnknownToolFailure(t *testing.T) {
	daemon, buffer := newUsageHarness(t, &fakeExecutor{})

	response := daemon.invoke(context.Background(), relay.InvokeRequest{
		Type:      relay.FrameInvoke,
		Tool:      "ghost",
		Operation: "ping",
		Input:     map[string]any{},
	})
	if response.Success {
		t.Fatal("invoke of an unregistered tool unexpectedly succeeded")
	}

	events := recordedEvents(t, buffer)
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Tool != "ghost" || events[0].ErrorCode != string(relay.CodeToolNotFound) {
		t.Errorf("event = %+v, want ghost/TOOL_NOT_FOUND", events[0])
	}
}

// The event surface is the privacy guarantee: it has no field for input values,
// headers, bodies, or responses, so a secret cannot reach telemetry even by
// accident (spec §32). This asserts the wire shape rather than the struct, so a
// new field cannot be added without this test failing.
func TestUsageEventCarriesNoPayload(t *testing.T) {
	executor := &fakeExecutor{response: protocol.Response{Status: 200, Body: map[string]any{"ok": true}}}
	daemon, buffer := newUsageHarness(t, executor)
	invoke(t, daemon)

	allowed := map[string]bool{
		"tool": true, "operation": true, "timestamp": true,
		"durationMs": true, "success": true, "errorCode": true,
	}
	line := strings.TrimSpace(buffer.String())
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("usage line is not JSON: %v (%q)", err, line)
	}
	for field := range fields {
		if !allowed[field] {
			t.Errorf("usage event carries unexpected field %q", field)
		}
	}
}

func TestTelemetryDisabledWritesNothing(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	if err := registry.New(layout.Registry).Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    writeToolScript(t, noAuthManifest),
		Runtime: "relay/v1",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	daemon := New(Config{
		Layout:    layout,
		Version:   "test",
		Executors: map[string]protocol.Executor{"rest": &fakeExecutor{response: protocol.Response{Status: 200}}},
		Keychain:  newMemKeychain(),
		Log:       io.Discard,
	})
	if response := invoke(t, daemon); !response.Success {
		t.Fatalf("invoke failed: %+v", response.Error)
	}
	if _, err := os.Stat(telemetry.File(layout)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("telemetry file exists with recording disabled (stat err = %v)", err)
	}
}

func TestTelemetryFailureDoesNotFailInvocation(t *testing.T) {
	daemon, _ := newUsageHarness(t, &fakeExecutor{response: protocol.Response{Status: 200}})
	daemon.usage = failingRecorder{}

	response := invoke(t, daemon)
	if !response.Success {
		t.Fatalf("a telemetry write error broke the invocation: %+v", response.Error)
	}
}
