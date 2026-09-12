package ipc

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"relay/pkg/relay"
)

// jsonKeys marshals a value and returns its raw top-level JSON keys, so a typo
// in a struct tag fails the test even when the decoded value would still match.
func jsonKeys(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("decode %T keys: %v", value, err)
	}
	return keys
}

// sortedKeys returns the key names in a stable order for comparison.
func sortedKeys(keys map[string]json.RawMessage) []string {
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// assertExactKeys pins the exact wire key set: a missing key silently breaks the
// CLI, and an unexpected key leaks shape the contract never promised.
func assertExactKeys(t *testing.T, value any, want ...string) {
	t.Helper()
	got := sortedKeys(jsonKeys(t, value))
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%T JSON keys = %v, want %v", value, got, want)
	}
}

// assertNoSecretFields fails when any given frame type exposes a field whose
// JSON key names a secret, so no frame can ever carry a token or verifier.
func assertNoSecretFields(t *testing.T, frames ...any) {
	t.Helper()
	forbidden := []string{"token", "verifier", "secret", "password", "code", "challenge"}
	for _, frame := range frames {
		typ := reflect.TypeOf(frame)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key := strings.ToLower(field.Tag.Get("json"))
			for _, bad := range forbidden {
				if strings.Contains(key, bad) {
					t.Errorf("%s.%s json key %q names %q, which must never cross IPC",
						typ.Name(), field.Name, key, bad)
				}
			}
		}
	}
}

// TestAuthBrowserFrameKinds pins the wire values the CLI and daemon both key
// off; a change here silently breaks the handshake.
func TestAuthBrowserFrameKinds(t *testing.T) {
	if FrameAuthBrowserStart != "auth_browser_start" {
		t.Errorf("FrameAuthBrowserStart = %q, want %q", FrameAuthBrowserStart, "auth_browser_start")
	}
	if FrameAuthBrowserWait != "auth_browser_wait" {
		t.Errorf("FrameAuthBrowserWait = %q, want %q", FrameAuthBrowserWait, "auth_browser_wait")
	}
}

// TestAuthBrowserStartRequestWireKeys covers the exact JSON keys the CLI writes.
func TestAuthBrowserStartRequestWireKeys(t *testing.T) {
	request := AuthBrowserStartRequest{Type: FrameAuthBrowserStart, Tool: "demo"}
	assertExactKeys(t, request, "type", "tool")

	keys := jsonKeys(t, request)
	var gotType, gotTool string
	if err := json.Unmarshal(keys["type"], &gotType); err != nil {
		t.Fatalf("decode type: %v", err)
	}
	if err := json.Unmarshal(keys["tool"], &gotTool); err != nil {
		t.Fatalf("decode tool: %v", err)
	}
	if gotType != FrameAuthBrowserStart || gotTool != "demo" {
		t.Fatalf("got type=%q tool=%q, want %q demo", gotType, gotTool, FrameAuthBrowserStart)
	}
}

// TestAuthBrowserStartResponseWireKeys covers the exact JSON keys of the reply
// that carries the URL to open, and that the values survive a round trip.
func TestAuthBrowserStartResponseWireKeys(t *testing.T) {
	response := AuthBrowserStartResponse{
		Success:     true,
		Tool:        "demo",
		Flow:        "flow-1",
		URL:         "https://example.test/authorize?state=s",
		RedirectURI: "http://127.0.0.1:7777/callback",
	}
	assertExactKeys(t, response, "success", "tool", "flow", "url", "redirectUri")

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var decoded AuthBrowserStartResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(decoded, response) {
		t.Fatalf("round trip changed the response:\n got %+v\nwant %+v", decoded, response)
	}
}

// TestAuthBrowserStartResponseOmitsEmptyMetadata covers omitempty: a failed
// start must not carry a stale handle or URL, and an empty reply is just success.
func TestAuthBrowserStartResponseOmitsEmptyMetadata(t *testing.T) {
	failure := AuthBrowserStartResponse{
		Success: false,
		Error:   relay.NewError(relay.CodeAuthFailed, "browser authorization was denied"),
	}
	assertExactKeys(t, failure, "success", "error")

	assertExactKeys(t, AuthBrowserStartResponse{}, "success")
}

// TestAuthBrowserWaitWireKeys covers both frames of the wait half.
func TestAuthBrowserWaitWireKeys(t *testing.T) {
	request := AuthBrowserWaitRequest{Type: FrameAuthBrowserWait, Flow: "flow-1"}
	assertExactKeys(t, request, "type", "flow")

	response := AuthBrowserWaitResponse{Success: true, Tool: "demo", Stored: true}
	assertExactKeys(t, response, "success", "tool", "stored")

	assertExactKeys(t, AuthBrowserWaitResponse{}, "success")
}

// TestAuthBrowserFramesCarryNoSecret asserts structurally that no browser frame
// names a token, verifier, code, or other secret field (spec §22).
func TestAuthBrowserFramesCarryNoSecret(t *testing.T) {
	assertNoSecretFields(t,
		AuthBrowserStartRequest{},
		AuthBrowserStartResponse{},
		AuthBrowserWaitRequest{},
		AuthBrowserWaitResponse{},
	)
}
