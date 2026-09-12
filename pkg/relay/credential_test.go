package relay

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// assertCredentialKeys pins the exact wire key set of an auth status reply, so a
// typo in a tag or an accidental non-omitempty field fails the test.
func assertCredentialKeys(t *testing.T, value AuthStatusResponse, want ...string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("decode %T keys: %v", value, err)
	}
	got := make([]string, 0, len(keys))
	for name := range keys {
		got = append(got, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%T JSON keys = %v, want %v", value, got, want)
	}
}

// TestAuthStatusResponseSummaryWireKeys covers the additive summary fields: the
// full reply exposes each under its exact key, and a legacy reply keeps exactly
// its old key set because every new field is omitempty (spec §22).
func TestAuthStatusResponseSummaryWireKeys(t *testing.T) {
	full := AuthStatusResponse{
		Success:     true,
		Tool:        "demo",
		Stored:      true,
		AuthType:    "oauth2",
		Provider:    "github",
		LoginKind:   "oauth2 device",
		ExpiresAt:   "2026-09-11T18:00:00Z",
		Scope:       "repo read:user",
		Refreshable: true,
	}
	assertCredentialKeys(t, full, "success", "tool", "stored", "authType", "provider", "loginKind", "expiresAt", "scope", "refreshable")

	legacy := AuthStatusResponse{Success: true, Tool: "demo", Stored: true}
	assertCredentialKeys(t, legacy, "success", "tool", "stored")
}

// TestAuthStatusResponseSummaryRoundTrips covers that the summary metadata
// survives a marshal/unmarshal round trip.
func TestAuthStatusResponseSummaryRoundTrips(t *testing.T) {
	response := AuthStatusResponse{
		Success:     true,
		Tool:        "demo",
		Stored:      true,
		LoginKind:   "oauth2 browser",
		ExpiresAt:   "2026-09-11T18:00:00Z",
		Scope:       "repo",
		Refreshable: true,
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var decoded AuthStatusResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(decoded, response) {
		t.Fatalf("round trip changed the response:\n got %+v\nwant %+v", decoded, response)
	}
}

// TestAuthStatusResponseNeverGainsSecretKeys asserts the reply type names no
// secret field: the summary is metadata, and the value itself never appears
// (spec §22).
func TestAuthStatusResponseNeverGainsSecretKeys(t *testing.T) {
	forbidden := []string{"token", "secret", "password", "code", "verifier"}
	typ := reflect.TypeOf(AuthStatusResponse{})
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
