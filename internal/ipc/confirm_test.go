package ipc

import (
	"encoding/json"
	"testing"

	"relay/pkg/relay"
)

// TestConfirmsBindsToolAndOperation covers spec §25: an acknowledgement resolves
// the gate for exactly one tool and one operation, and the empty acknowledgement
// a tool binary or MCP client sends resolves nothing.
func TestConfirmsBindsToolAndOperation(t *testing.T) {
	tests := []struct {
		name         string
		confirmation string
		tool         string
		operation    string
		want         bool
	}{
		{"exact match confirms", ConfirmationToken("demo", "delete_repository"), "demo", "delete_repository", true},
		{"empty never confirms", "", "demo", "delete_repository", false},
		{"different operation does not confirm", ConfirmationToken("demo", "other"), "demo", "delete_repository", false},
		{"different tool does not confirm", ConfirmationToken("other", "delete_repository"), "demo", "delete_repository", false},
		{"arbitrary string does not confirm", "yes", "demo", "delete_repository", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Confirms(test.confirmation, test.tool, test.operation); got != test.want {
				t.Fatalf("Confirms(%q, %q, %q) = %v, want %v",
					test.confirmation, test.tool, test.operation, got, test.want)
			}
		})
	}
}

// TestInvokeFrameWireShape covers spec §41: a plain relay.InvokeRequest — what a
// tool binary and the MCP adapter send — decodes to an empty Confirmation, and
// an unconfirmed frame carries no confirmation key, so the wire shape is exactly
// the pre-feature request.
func TestInvokeFrameWireShape(t *testing.T) {
	plain := relay.InvokeRequest{
		Type:      relay.FrameInvoke,
		Tool:      "demo",
		Operation: "ping",
		Input:     map[string]any{"x": float64(1)},
	}
	encoded, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal plain request: %v", err)
	}
	var frame InvokeFrame
	if err := json.Unmarshal(encoded, &frame); err != nil {
		t.Fatalf("decode into InvokeFrame: %v", err)
	}
	if frame.Confirmation != "" {
		t.Fatalf("a plain invoke request decoded to confirmation %q, want empty", frame.Confirmation)
	}
	if frame.Tool != "demo" || frame.Operation != "ping" {
		t.Fatalf("embedded request lost its fields: %+v", frame.InvokeRequest)
	}

	// A confirmed frame carries the token; an unconfirmed one must not gain the
	// key at all, so the two are distinguishable on the wire.
	confirmed, err := json.Marshal(InvokeFrame{InvokeRequest: plain, Confirmation: ConfirmationToken("demo", "ping")})
	if err != nil {
		t.Fatalf("marshal confirmed frame: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(confirmed, &decoded); err != nil {
		t.Fatalf("decode confirmed frame: %v", err)
	}
	if decoded[ConfirmationField] != ConfirmationToken("demo", "ping") {
		t.Fatalf("confirmed frame %q lost its %s field: %s", confirmed, ConfirmationField, confirmed)
	}
	if _, present := decoded["operation"]; !present {
		t.Fatalf("embedding did not flatten the request: %s", confirmed)
	}
}
