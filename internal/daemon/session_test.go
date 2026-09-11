package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"relay/internal/browser"
	"relay/internal/ipc"
	"relay/internal/keychain"
	"relay/internal/paths"
	"relay/internal/protocol"
	browserproto "relay/internal/protocol/browser"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// browserManifestTemplate is a browser tool with a form login. %s is the base
// URL, so a test can point it at an httptest server (spec §23).
const browserManifestTemplate = "apiVersion: relay/v1\n" +
	"kind: Tool\n" +
	"metadata:\n" +
	"  name: demo\n" +
	"  version: 1.0.0\n" +
	"runtime:\n" +
	"  name: relay\n" +
	"  apiVersion: v1\n" +
	"protocol:\n" +
	"  type: browser\n" +
	"  baseUrl: %s\n" +
	"auth:\n" +
	"  type: basic\n" +
	"  login:\n" +
	"    kind: form\n" +
	"    path: /login\n" +
	"    usernameField: user\n" +
	"    passwordField: pass\n" +
	"tools:\n" +
	"  - name: ping\n" +
	"    description: ping the service with the stored session\n" +
	"    input:\n" +
	"      type: object\n" +
	"    request:\n" +
	"      method: GET\n" +
	"      path: /ping\n"

// sessionRecorder records what the loopback server saw.
type sessionRecorder struct {
	mu      sync.Mutex
	logins  int
	cookies []string
}

func (r *sessionRecorder) addLogin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logins++
}

func (r *sessionRecorder) addPing(cookie string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cookies = append(r.cookies, cookie)
}

func (r *sessionRecorder) loginCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.logins
}

func (r *sessionRecorder) pingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cookies)
}

func (r *sessionRecorder) lastCookie() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cookies) == 0 {
		return ""
	}
	return r.cookies[len(r.cookies)-1]
}

// sessionServer serves a form login that sets a cookie and a protected ping that
// echoes back the cookie it was sent.
func sessionServer(t *testing.T) (*httptest.Server, *sessionRecorder) {
	t.Helper()
	rec := &sessionRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login":
			rec.addLogin()
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-abc", Path: "/"})
			fmt.Fprint(w, "{\"ok\":true}")
		case "/ping":
			cookie := ""
			if c, err := r.Cookie("sid"); err == nil {
				cookie = c.Value
			}
			rec.addPing(cookie)
			fmt.Fprint(w, "{\"ok\":true}")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newBrowserDaemon builds a daemon over one registered browser tool whose
// session store is shared with its executor, the way relayd wires them.
func newBrowserDaemon(t *testing.T, srv *httptest.Server) (*Daemon, *memKeychain) {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	manifestYAML := fmt.Sprintf(browserManifestTemplate, srv.URL)
	if err := registry.New(layout.Registry).Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    writeToolScript(t, manifestYAML),
		Runtime: "relay/v1",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	kc := newMemKeychain()
	sessions := browser.New(browser.Config{
		Dir:       browser.SessionDir(layout),
		Keychain:  kc,
		Transport: srv.Client().Transport,
	})
	return New(Config{
		Layout:    layout,
		Version:   "test",
		Executors: map[string]protocol.Executor{"browser": browserproto.New(sessions)},
		Keychain:  kc,
		Sessions:  sessions,
		Log:       io.Discard,
	}), kc
}

func pingInvoke() relay.InvokeRequest {
	return relay.InvokeRequest{Type: relay.FrameInvoke, Tool: "demo", Operation: "ping", Input: map[string]any{}}
}

func sendFrame(t *testing.T, d *Daemon, kind string, payload any) any {
	t.Helper()
	frame, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s frame: %v", kind, err)
	}
	reply, err := d.Handle(context.Background(), kind, frame)
	if err != nil {
		t.Fatalf("Handle %s: %v", kind, err)
	}
	return reply
}

// TestSessionLoginThenOperationUsesCookie drives the whole browser-session seam
// through the daemon Handle path: an operation with no session is AUTH_REQUIRED,
// a session_login establishes one the executor then sends, and session_clear
// revokes it (spec §23).
func TestSessionLoginThenOperationUsesCookie(t *testing.T) {
	srv, rec := sessionServer(t)
	d, kc := newBrowserDaemon(t, srv)
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, "alice:secret"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	// A stored credential is not a session: a browser operation must still ask
	// for a login, not report the credential as refused.
	before := d.invoke(context.Background(), pingInvoke())
	if before.Success || before.Error == nil || before.Error.Code != relay.CodeAuthRequired {
		t.Fatalf("before login: success=%v error=%+v, want AUTH_REQUIRED", before.Success, before.Error)
	}
	if rec.pingCount() != 0 {
		t.Fatal("the service was contacted before a session existed")
	}

	reply := sendFrame(t, d, ipc.FrameSessionLogin, ipc.SessionLoginRequest{
		Type: ipc.FrameSessionLogin, Tool: "demo",
	})
	login, ok := reply.(ipc.SessionLoginResponse)
	if !ok {
		t.Fatalf("login reply type = %T, want ipc.SessionLoginResponse", reply)
	}
	if !login.Success || !login.LoggedIn {
		t.Fatalf("login reply = %+v, want success with LoggedIn", login)
	}
	if rec.loginCount() != 1 {
		t.Fatalf("login endpoint hit %d times, want 1", rec.loginCount())
	}
	encoded, err := json.Marshal(login)
	if err != nil {
		t.Fatalf("marshal login reply: %v", err)
	}
	if strings.Contains(string(encoded), "session-abc") {
		t.Fatalf("login reply leaked the session cookie: %s", encoded)
	}

	after := d.invoke(context.Background(), pingInvoke())
	if !after.Success {
		t.Fatalf("operation after login failed: %v", after.Error)
	}
	if got := rec.lastCookie(); got != "session-abc" {
		t.Fatalf("operation sent cookie %q, want the stored session", got)
	}

	clearedReply := sendFrame(t, d, ipc.FrameSessionClear, ipc.SessionClearRequest{
		Type: ipc.FrameSessionClear, Tool: "demo",
	})
	cleared, ok := clearedReply.(ipc.SessionClearResponse)
	if !ok {
		t.Fatalf("clear reply type = %T, want ipc.SessionClearResponse", clearedReply)
	}
	if !cleared.Success || !cleared.Cleared {
		t.Fatalf("clear reply = %+v, want success with Cleared", cleared)
	}

	revoked := d.invoke(context.Background(), pingInvoke())
	if revoked.Success || revoked.Error == nil || revoked.Error.Code != relay.CodeAuthRequired {
		t.Fatalf("after clear: success=%v error=%+v, want AUTH_REQUIRED", revoked.Success, revoked.Error)
	}
	if rec.pingCount() != 1 {
		t.Fatalf("a revoked session still contacted the service: pings=%d", rec.pingCount())
	}
}

// TestSessionFramesValidateTheTool proves a session frame refuses a nameless or
// unknown tool before touching the store (spec §23).
func TestSessionFramesValidateTheTool(t *testing.T) {
	srv, _ := sessionServer(t)
	d, _ := newBrowserDaemon(t, srv)

	loginEmpty := sendFrame(t, d, ipc.FrameSessionLogin, ipc.SessionLoginRequest{
		Type: ipc.FrameSessionLogin, Tool: "  ",
	}).(ipc.SessionLoginResponse)
	if loginEmpty.Success || loginEmpty.Error == nil || loginEmpty.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("empty login tool = %+v, want INVALID_INPUT", loginEmpty)
	}

	loginUnknown := sendFrame(t, d, ipc.FrameSessionLogin, ipc.SessionLoginRequest{
		Type: ipc.FrameSessionLogin, Tool: "ghost",
	}).(ipc.SessionLoginResponse)
	if loginUnknown.Success || loginUnknown.Error == nil || loginUnknown.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("unknown login tool = %+v, want TOOL_NOT_FOUND", loginUnknown)
	}

	clearEmpty := sendFrame(t, d, ipc.FrameSessionClear, ipc.SessionClearRequest{
		Type: ipc.FrameSessionClear, Tool: "",
	}).(ipc.SessionClearResponse)
	if clearEmpty.Success || clearEmpty.Error == nil || clearEmpty.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("empty clear tool = %+v, want INVALID_INPUT", clearEmpty)
	}

	clearUnknown := sendFrame(t, d, ipc.FrameSessionClear, ipc.SessionClearRequest{
		Type: ipc.FrameSessionClear, Tool: "ghost",
	}).(ipc.SessionClearResponse)
	if clearUnknown.Success || clearUnknown.Error == nil || clearUnknown.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("unknown clear tool = %+v, want TOOL_NOT_FOUND", clearUnknown)
	}
}

// TestSessionFramesRequireAStore proves a daemon without a session store refuses
// the frames rather than pretending a login happened.
func TestSessionFramesRequireAStore(t *testing.T) {
	d := newDaemonHarness(t, noAuthManifest, &fakeExecutor{}, newMemKeychain())

	login := sendFrame(t, d, ipc.FrameSessionLogin, ipc.SessionLoginRequest{
		Type: ipc.FrameSessionLogin, Tool: "demo",
	}).(ipc.SessionLoginResponse)
	if login.Success || login.Error == nil || login.Error.Code != relay.CodeProtocolError {
		t.Fatalf("login without a store = %+v, want PROTOCOL_ERROR", login)
	}

	clear := sendFrame(t, d, ipc.FrameSessionClear, ipc.SessionClearRequest{
		Type: ipc.FrameSessionClear, Tool: "demo",
	}).(ipc.SessionClearResponse)
	if clear.Success || clear.Error == nil || clear.Error.Code != relay.CodeProtocolError {
		t.Fatalf("clear without a store = %+v, want PROTOCOL_ERROR", clear)
	}
}
