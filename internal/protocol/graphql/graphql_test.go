package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"relay/internal/protocol"
	"relay/pkg/relay"
)

const getThingDocument = "query GetThing($id: ID!) { thing(id: $id) { id name } }"

// TestExecute is the table-driven protocol contract: success, variable
// substitution, GraphQL-level errors, every mapped status code, transport
// failure, and context timeout, all against httptest with no network.
func TestExecute(t *testing.T) {
	tests := []struct {
		name       string
		document   string
		variables  map[string]string
		input      map[string]any
		credential *protocol.Credential
		offline    bool
		timeout    time.Duration
		handler    func(*testing.T, http.ResponseWriter, *http.Request)
		verify     func(*testing.T, protocol.Response, error)
	}{
		{
			name:     "success returns the data member",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("expected POST, got %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "{\"data\":{\"thing\":{\"id\":\"7\",\"name\":\"ada\"}}}")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				body, ok := resp.Body.(map[string]any)
				if !ok {
					t.Fatalf("expected object body, got %T", resp.Body)
				}
				thing, ok := body["thing"].(map[string]any)
				if !ok || thing["name"] != "ada" {
					t.Fatalf("unexpected body: %#v", resp.Body)
				}
			},
		},
		{
			name:     "resolves declared variables from input",
			document: getThingDocument,
			input:    map[string]any{"id": "42"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				query, _ := payload["query"].(string)
				if !strings.Contains(query, "$id") {
					t.Errorf("query did not carry the document: %q", query)
				}
				variables, _ := payload["variables"].(map[string]any)
				if variables["id"] != "42" {
					t.Errorf("expected variables.id=42, got %#v", payload["variables"])
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "{\"data\":{\"ok\":true}}")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
		},
		{
			name:      "variables mapping renames onto an input property",
			document:  "query GetUser($userId: ID!) { user(id: $userId) { name } }",
			variables: map[string]string{"userId": "id"},
			input:     map[string]any{"id": "9"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				variables, _ := payload["variables"].(map[string]any)
				if variables["userId"] != "9" {
					t.Errorf("expected variables.userId=9, got %#v", payload["variables"])
				}
				if _, present := variables["id"]; present {
					t.Errorf("raw input name leaked as a variable: %#v", payload["variables"])
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "{\"data\":{\"ok\":true}}")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
		},
		{
			name:     "errors array becomes REMOTE_ERROR without partial data",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "{\"data\":{\"thing\":null},\"errors\":[{\"message\":\"boom\"},{\"message\":\"bang\"}]}")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				failure := wantCode(t, err, relay.CodeRemoteError)
				messages, _ := failure.Details["graphqlErrors"].([]string)
				if len(messages) != 2 || messages[0] != "boom" || messages[1] != "bang" {
					t.Fatalf("unexpected error details: %#v", failure.Details)
				}
				if resp.Body != nil {
					t.Errorf("expected no partial data, got %#v", resp.Body)
				}
			},
		},
		{
			name:     "unauthorized is AUTH_REQUIRED",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, "{\"errors\":[{\"message\":\"unauthorized\"}]}")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				wantCode(t, err, relay.CodeAuthRequired)
			},
		},
		{
			name:     "rate limited is RATE_LIMITED",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				wantCode(t, err, relay.CodeRateLimited)
			},
		},
		{
			name:     "server error is REMOTE_ERROR",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				wantCode(t, err, relay.CodeRemoteError)
			},
		},
		{
			name:     "unreachable endpoint is NETWORK_ERROR",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			offline:  true,
			verify: func(t *testing.T, resp protocol.Response, err error) {
				wantCode(t, err, relay.CodeNetworkError)
			},
		},
		{
			name:     "context deadline is TIMEOUT",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			timeout:  25 * time.Millisecond,
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				time.Sleep(250 * time.Millisecond)
				io.WriteString(w, "{\"data\":{}}")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				wantCode(t, err, relay.CodeTimeout)
			},
		},
		{
			name:     "credential is injected like REST",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			credential: &protocol.Credential{
				Type: "bearer", Secret: "tok", Scheme: "Bearer",
			},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer tok" {
					t.Errorf("expected injected credential, got %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "{\"data\":{\"ok\":true}}")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
		},
		{
			name:     "non-JSON success body is PROTOCOL_ERROR",
			document: getThingDocument,
			input:    map[string]any{"id": "7"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "not json")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				wantCode(t, err, relay.CodeProtocolError)
			},
		},
		{
			name:     "missing document is INVALID_INPUT",
			document: "",
			input:    map[string]any{"id": "7"},
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				t.Error("no request should be sent without a document")
			},
			verify: func(t *testing.T, resp protocol.Response, err error) {
				wantCode(t, err, relay.CodeInvalidInput)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.handler != nil {
					test.handler(t, w, r)
				}
			}))
			endpoint := server.URL
			if test.offline {
				server.Close()
			} else {
				defer server.Close()
			}

			ctx := context.Background()
			if test.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.timeout)
				defer cancel()
			}

			response, err := New().Execute(ctx, protocol.Request{
				Tool:       "demo",
				Operation:  "get_thing",
				Input:      test.input,
				Credential: test.credential,
				Spec: protocol.Spec{
					Type:      "graphql",
					Endpoint:  endpoint,
					Document:  test.document,
					Variables: test.variables,
				},
			})
			test.verify(t, response, err)
		})
	}
}

func wantCode(t *testing.T, err error, code relay.Code) *relay.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	var failure *relay.Error
	if !errors.As(err, &failure) {
		t.Fatalf("expected *relay.Error, got %T: %v", err, err)
	}
	if failure.Code != code {
		t.Fatalf("expected code %s, got %s (%s)", code, failure.Code, failure.Message)
	}
	return failure
}
