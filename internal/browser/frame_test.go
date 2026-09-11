package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relay/internal/ipc"
	"relay/internal/keychain"
)

// frameSecrets is a keychain double for the frame-seam tests.
type frameSecrets struct {
	secret string
}

func (f *frameSecrets) Get(service, account string) (string, error) {
	if f.secret == "" {
		return "", keychain.ErrNotFound
	}
	return f.secret, nil
}

func (f *frameSecrets) Set(service, account, secret string) error { f.secret = secret; return nil }
func (f *frameSecrets) Delete(service, account string) error      { f.secret = ""; return nil }

// frameLoginManifest declares a form login without relying on a raw string
// literal, so the embedded newlines stay explicit.
func frameLoginManifest(baseURL string) []byte {
	return []byte(fmt.Sprintf(
		"protocol:\n  type: browser\n  baseUrl: %s\n"+
			"auth:\n  type: basic\n  login:\n    kind: form\n    path: /login\n"+
			"    usernameField: user\n    passwordField: pass\n", baseURL))
}

// TestHandleLoginStoresSessionAndLeaksNothing drives the session_login frame's
// body directly: a valid login reports success and stores the session, and the
// reply JSON carries neither the cookie nor the credential (spec §22, §23).
func TestHandleLoginStoresSessionAndLeaksNothing(t *testing.T) {
	const cookieValue = "session-abc"
	const credential = "alice:wonderland"
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: cookieValue, Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sessions := New(Config{Dir: t.TempDir(), Keychain: &frameSecrets{secret: credential}})
	response := sessions.HandleLogin(context.Background(),
		ipc.SessionLoginRequest{Type: ipc.FrameSessionLogin, Tool: "demo"},
		frameLoginManifest(srv.URL))
	if !response.Success || !response.LoggedIn {
		t.Fatalf("response = %+v, want a logged-in success", response)
	}
	has, err := sessions.Store().Has("demo")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !has {
		t.Fatal("HandleLogin should have stored the session")
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(encoded), cookieValue) || strings.Contains(string(encoded), credential) {
		t.Fatalf("reply leaked a secret: %s", encoded)
	}
}

// TestHandleLoginReportsFailureStructured proves a login failure is returned as
// a structured error inside a normal reply, never as a panic or a bare string
// (spec §26).
func TestHandleLoginReportsFailureStructured(t *testing.T) {
	sessions := New(Config{Dir: t.TempDir(), Keychain: &frameSecrets{}})
	response := sessions.HandleLogin(context.Background(),
		ipc.SessionLoginRequest{Type: ipc.FrameSessionLogin, Tool: "demo"},
		frameLoginManifest("https://api.example.com"))
	if response.Success || response.Error == nil {
		t.Fatalf("response = %+v, want a structured failure", response)
	}
}

// TestHandleClearForgetsSession proves the session_clear frame removes the
// session and reports it idempotently (spec §23).
func TestHandleClearForgetsSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-abc", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sessions := New(Config{Dir: t.TempDir(), Keychain: &frameSecrets{secret: "alice:wonderland"}})
	if response := sessions.HandleLogin(context.Background(),
		ipc.SessionLoginRequest{Tool: "demo"}, frameLoginManifest(srv.URL)); !response.Success {
		t.Fatalf("HandleLogin = %+v, want success", response)
	}

	cleared := sessions.HandleClear(ipc.SessionClearRequest{Type: ipc.FrameSessionClear, Tool: "demo"})
	if !cleared.Success || !cleared.Cleared {
		t.Fatalf("HandleClear = %+v, want cleared success", cleared)
	}
	has, err := sessions.Store().Has("demo")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if has {
		t.Fatal("session should be gone after HandleClear")
	}
	// Clearing again is still a success.
	if again := sessions.HandleClear(ipc.SessionClearRequest{Tool: "demo"}); !again.Success || !again.Cleared {
		t.Fatalf("second HandleClear = %+v, want idempotent success", again)
	}
}
