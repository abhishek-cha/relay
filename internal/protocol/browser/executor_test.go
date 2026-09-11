package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"relay/internal/browser"
	"relay/internal/keychain"
	"relay/internal/protocol"
	"relay/pkg/relay"
)

// fakeSecrets is an in-process keychain double. The session layer reads its
// login credential through the same keychain.Store interface the daemon uses,
// so no real Keychain is touched (spec §40).
type fakeSecrets struct {
	secrets map[string]string
}

func (f *fakeSecrets) Get(service, account string) (string, error) {
	secret, ok := f.secrets[service]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return secret, nil
}

func (f *fakeSecrets) Set(service, account, secret string) error {
	if f.secrets == nil {
		f.secrets = map[string]string{}
	}
	f.secrets[service] = secret
	return nil
}

func (f *fakeSecrets) Delete(service, account string) error {
	delete(f.secrets, service)
	return nil
}

func newTestSessions(t *testing.T, secrets keychain.Store) *browser.Sessions {
	t.Helper()
	return browser.New(browser.Config{Dir: t.TempDir(), Keychain: secrets})
}

// formLoginManifest declares a form login against baseURL, matching the shape
// the manifest schema accepts for a browser tool (spec §23).
func formLoginManifest(baseURL string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 0.1.0
protocol:
  type: browser
  baseUrl: %s
auth:
  type: basic
  login:
    kind: form
    path: /login
    usernameField: user
    passwordField: pass
`, baseURL))
}

func structuredError(t *testing.T, err error) *relay.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var structured *relay.Error
	if !errors.As(err, &structured) {
		t.Fatalf("error %v is not a *relay.Error", err)
	}
	return structured
}

// TestFormLoginEstablishesSessionForLaterOperation is the end-to-end path: a
// form login posts the Keychain credential, the service sets a cookie, and a
// later operation reuses that cookie over the same REST-shaped request the CLI
// and MCP already send (spec §19, §22, §23).
func TestFormLoginEstablishesSessionForLaterOperation(t *testing.T) {
	const cookieValue = "session-abc"
	var (
		loginSawUser  string
		loginSawPass  string
		dataSawCookie string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		loginSawUser = r.PostFormValue("user")
		loginSawPass = r.PostFormValue("pass")
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: cookieValue, Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sid")
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		dataSawCookie = cookie.Value
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	secrets := &fakeSecrets{secrets: map[string]string{keychain.Service("demo"): "alice:wonderland"}}
	sessions := newTestSessions(t, secrets)
	executor := New(sessions)
	ctx := context.Background()

	if failure := sessions.Login(ctx, "demo", formLoginManifest(srv.URL)); failure != nil {
		t.Fatalf("Login: %v", failure)
	}
	if loginSawUser != "alice" || loginSawPass != "wonderland" {
		t.Fatalf("login saw %q/%q, want the stored credential", loginSawUser, loginSawPass)
	}
	has, err := sessions.Store().Has("demo")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !has {
		t.Fatal("login should have stored a session")
	}

	response, err := executor.Execute(ctx, protocol.Request{
		Tool:      "demo",
		Operation: "data",
		Spec:      protocol.Spec{Type: "browser", BaseURL: srv.URL, Method: "GET", Path: "/data"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if response.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Status)
	}
	if dataSawCookie != cookieValue {
		t.Fatalf("operation sent cookie %q, want %q", dataSawCookie, cookieValue)
	}
	body, ok := response.Body.(map[string]any)
	if !ok || body["ok"] != true {
		t.Fatalf("body = %#v, want {ok:true}", response.Body)
	}
}

// TestOperationWithoutSessionIsAuthRequired covers the honest failure: a
// browser operation with no session is refused as AUTH_REQUIRED and never
// reaches the service (spec §23, §26).
func TestOperationWithoutSessionIsAuthRequired(t *testing.T) {
	var contacted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
	}))
	defer srv.Close()

	sessions := newTestSessions(t, &fakeSecrets{})
	executor := New(sessions)
	_, err := executor.Execute(context.Background(), protocol.Request{
		Tool:      "demo",
		Operation: "data",
		Spec:      protocol.Spec{Type: "browser", BaseURL: srv.URL, Method: "GET", Path: "/data"},
	})
	structured := structuredError(t, err)
	if structured.Code != relay.CodeAuthRequired {
		t.Fatalf("code = %s, want %s", structured.Code, relay.CodeAuthRequired)
	}
	if contacted {
		t.Fatal("an operation without a session must not contact the service")
	}
}

// TestClearedSessionFailsAsAuthRequired proves clearing is real: after Clear,
// the same operation fails the same way a never-logged-in tool does
// (spec §23).
func TestClearedSessionFailsAsAuthRequired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-abc", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	secrets := &fakeSecrets{secrets: map[string]string{keychain.Service("demo"): "alice:wonderland"}}
	sessions := newTestSessions(t, secrets)
	executor := New(sessions)
	ctx := context.Background()
	manifest := formLoginManifest(srv.URL)

	if failure := sessions.Login(ctx, "demo", manifest); failure != nil {
		t.Fatalf("Login: %v", failure)
	}
	if failure := executor.Clear("demo"); failure != nil {
		t.Fatalf("Clear: %v", failure)
	}
	_, err := executor.Execute(ctx, protocol.Request{
		Tool:      "demo",
		Operation: "data",
		Spec:      protocol.Spec{Type: "browser", BaseURL: srv.URL, Method: "GET", Path: "/data"},
	})
	structured := structuredError(t, err)
	if structured.Code != relay.CodeAuthRequired {
		t.Fatalf("code = %s, want %s", structured.Code, relay.CodeAuthRequired)
	}
}

// TestLoginWithoutCookieIsAuthFailed covers the login that is answered 2xx but
// establishes nothing. Reporting success would surface later as a confusing
// AUTH_REQUIRED, so the missing session is the failure (spec §23, §26).
func TestLoginWithoutCookieIsAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secrets := &fakeSecrets{secrets: map[string]string{keychain.Service("demo"): "alice:wonderland"}}
	sessions := newTestSessions(t, secrets)
	failure := sessions.Login(context.Background(), "demo", formLoginManifest(srv.URL))
	if failure == nil || failure.Code != relay.CodeAuthFailed {
		t.Fatalf("failure = %v, want AUTH_FAILED", failure)
	}
}

// TestLoginRejectsMalformedBlockAtExecutor proves the daemon-level entry point
// rejects a half-written login block instead of guessing (spec §26).
func TestLoginRejectsMalformedBlockAtExecutor(t *testing.T) {
	secrets := &fakeSecrets{secrets: map[string]string{keychain.Service("demo"): "alice:wonderland"}}
	sessions := newTestSessions(t, secrets)
	malformed := []byte("protocol:\n  baseUrl: https://api.example.com\nauth:\n  login:\n    kind: form\n    path: /login\n")
	failure := sessions.Login(context.Background(), "demo", malformed)
	if failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("failure = %v, want INVALID_INPUT", failure)
	}
}

// TestResponseOverCapIsRejected proves the 8 MiB body cap is enforced so a
// hostile backend cannot exhaust daemon memory (spec §40).
func TestResponseOverCapIsRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-abc", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxResponseBodyBytes+1))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	secrets := &fakeSecrets{secrets: map[string]string{keychain.Service("demo"): "alice:wonderland"}}
	sessions := newTestSessions(t, secrets)
	executor := New(sessions)
	ctx := context.Background()
	if failure := sessions.Login(ctx, "demo", formLoginManifest(srv.URL)); failure != nil {
		t.Fatalf("Login: %v", failure)
	}

	_, err := executor.Execute(ctx, protocol.Request{
		Tool:      "demo",
		Operation: "data",
		Spec:      protocol.Spec{Type: "browser", BaseURL: srv.URL, Method: "GET", Path: "/data"},
	})
	structured := structuredError(t, err)
	if structured.Code != relay.CodeRemoteError {
		t.Fatalf("code = %s, want %s", structured.Code, relay.CodeRemoteError)
	}
}

// TestCrossHostRedirectDoesNotLeakSession proves the executor uses the guarded
// client: an operation that is redirected to a second host stops at the 3xx and
// never replays the session cookie (spec §23, §40).
func TestCrossHostRedirectDoesNotLeakSession(t *testing.T) {
	var otherContacted bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherContacted = true
	}))
	defer other.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-abc", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	secrets := &fakeSecrets{secrets: map[string]string{keychain.Service("demo"): "alice:wonderland"}}
	sessions := newTestSessions(t, secrets)
	executor := New(sessions)
	ctx := context.Background()
	if failure := sessions.Login(ctx, "demo", formLoginManifest(srv.URL)); failure != nil {
		t.Fatalf("Login: %v", failure)
	}

	_, err := executor.Execute(ctx, protocol.Request{
		Tool:      "demo",
		Operation: "data",
		Spec:      protocol.Spec{Type: "browser", BaseURL: srv.URL, Method: "GET", Path: "/data"},
	})
	structured := structuredError(t, err)
	if structured.Code != relay.CodeRemoteError {
		t.Fatalf("code = %s, want %s for the unfollowed 302", structured.Code, relay.CodeRemoteError)
	}
	if otherContacted {
		t.Fatal("the cross-host redirect target was contacted with the session")
	}
}

// TestExecuteBuildsRESTShapedRequest keeps the operation shape identical to
// REST: method, path placeholder, query, headers, and JSON body all resolve the
// same way, so the CLI and MCP need no browser-specific path (spec §19, §23).
func TestExecuteBuildsRESTShapedRequest(t *testing.T) {
	var gotPath, gotQuery, gotHeader, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-abc", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/items/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("limit")
		gotHeader = r.Header.Get("X-Trace")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	secrets := &fakeSecrets{secrets: map[string]string{keychain.Service("demo"): "alice:wonderland"}}
	sessions := newTestSessions(t, secrets)
	executor := New(sessions)
	ctx := context.Background()
	manifest := []byte(fmt.Sprintf("protocol:\n  type: browser\n  baseUrl: %s\nauth:\n  type: basic\n  login:\n    kind: form\n    path: /login\n    usernameField: user\n    passwordField: pass\n", srv.URL))
	if failure := sessions.Login(ctx, "demo", manifest); failure != nil {
		t.Fatalf("Login: %v", failure)
	}

	_, err := executor.Execute(ctx, protocol.Request{
		Tool:      "demo",
		Operation: "create",
		Input:     map[string]any{"id": "42", "limit": 5, "traceId": "trace-1", "name": "widget"},
		Spec: protocol.Spec{
			Type:    "browser",
			BaseURL: srv.URL,
			Method:  "POST",
			Path:    "/items/{id}",
			Query:   map[string]string{"limit": "{limit}"},
			Headers: map[string]string{"X-Trace": "{traceId}"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotPath != "/items/42" {
		t.Errorf("path = %q, want /items/42", gotPath)
	}
	if gotQuery != "5" {
		t.Errorf("query limit = %q, want 5", gotQuery)
	}
	if gotHeader != "trace-1" {
		t.Errorf("header X-Trace = %q, want trace-1", gotHeader)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", gotBody, err)
	}
	if body["name"] != "widget" || body["id"] != nil || body["limit"] != nil {
		t.Fatalf("body = %#v, want only the unconsumed name field", body)
	}
}
