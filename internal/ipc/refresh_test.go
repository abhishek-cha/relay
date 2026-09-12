package ipc

import (
	"encoding/json"
	"reflect"
	"testing"

	"relay/pkg/relay"
)

// TestAuthRefreshFrameKind pins the wire value of the refresh frame.
func TestAuthRefreshFrameKind(t *testing.T) {
	if FrameAuthRefresh != "auth_refresh" {
		t.Errorf("FrameAuthRefresh = %q, want %q", FrameAuthRefresh, "auth_refresh")
	}
}

// TestAuthRefreshRequestWireKeys covers the exact JSON keys for the one-tool,
// every-tool, and forced shapes, and that the unused flags stay off the wire.
func TestAuthRefreshRequestWireKeys(t *testing.T) {
	forced := AuthRefreshRequest{Type: FrameAuthRefresh, Tool: "demo", All: true, Force: true}
	assertExactKeys(t, forced, "type", "tool", "all", "force")

	one := AuthRefreshRequest{Type: FrameAuthRefresh, Tool: "demo"}
	assertExactKeys(t, one, "type", "tool")

	sweep := AuthRefreshRequest{Type: FrameAuthRefresh, All: true}
	assertExactKeys(t, sweep, "type", "all")
}

// TestAuthRefreshResultWireKeys covers the exact JSON keys of one per-tool
// result in each of its outcomes: refreshed, skipped, and failed.
func TestAuthRefreshResultWireKeys(t *testing.T) {
	fresh := AuthRefreshResult{Tool: "demo", Refreshed: true, ExpiresAt: "2026-09-11T18:00:00Z"}
	assertExactKeys(t, fresh, "tool", "refreshed", "expiresAt")

	skipped := AuthRefreshResult{Tool: "demo", Skipped: "token is not near expiry"}
	assertExactKeys(t, skipped, "tool", "refreshed", "skipped")

	failed := AuthRefreshResult{Tool: "demo", Error: relay.NewError(relay.CodeAuthFailed, "refresh was rejected")}
	assertExactKeys(t, failed, "tool", "refreshed", "error")
}

// TestAuthRefreshResponseRoundTripsSeveralResults covers the slice reply that
// serves both `relay auth refresh <tool>` and `relay auth refresh --all`.
func TestAuthRefreshResponseRoundTripsSeveralResults(t *testing.T) {
	response := AuthRefreshResponse{
		Success: true,
		Results: []AuthRefreshResult{
			{Tool: "alpha", Refreshed: true, ExpiresAt: "2026-09-11T18:00:00Z"},
			{Tool: "beta", Skipped: "token is not near expiry"},
			{Tool: "gamma", Error: relay.NewError(relay.CodeAuthFailed, "refresh was rejected")},
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var decoded AuthRefreshResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(decoded, response) {
		t.Fatalf("round trip changed the response:\n got %+v\nwant %+v", decoded, response)
	}
	if len(decoded.Results) != 3 {
		t.Fatalf("decoded %d results, want 3", len(decoded.Results))
	}

	assertExactKeys(t, response, "success", "results")
	assertExactKeys(t, AuthRefreshResponse{}, "success")
}

// TestAuthRefreshFramesCarryNoSecret asserts structurally that no refresh frame
// names a token, verifier, or other secret field (spec §22).
func TestAuthRefreshFramesCarryNoSecret(t *testing.T) {
	assertNoSecretFields(t,
		AuthRefreshRequest{},
		AuthRefreshResult{},
		AuthRefreshResponse{},
	)
}
