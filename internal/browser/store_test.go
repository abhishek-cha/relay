package browser

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

// TestStorePersistsAndReloadsCookies proves a session survives the daemon
// restarting: what one Store writes under the Relay home, a fresh Store reads
// back as the same live cookies (spec §15, §23).
func TestStorePersistsAndReloadsCookies(t *testing.T) {
	dir := t.TempDir()
	origin := mustURL(t, "https://api.example.com/")

	store := NewStore(dir)
	session, err := store.Jar("demo")
	if err != nil {
		t.Fatalf("Jar: %v", err)
	}
	session.SetCookies(origin, []*http.Cookie{{Name: "sid", Value: "cookie-value", Path: "/"}})
	if !session.HasCookies() {
		t.Fatal("session should report a live cookie before saving")
	}
	if err := session.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A different Store instance models the daemon reading its state back.
	reloaded, err := NewStore(dir).Jar("demo")
	if err != nil {
		t.Fatalf("reload Jar: %v", err)
	}
	if !reloaded.HasCookies() {
		t.Fatal("reloaded session should report a live cookie")
	}
	cookies := reloaded.Cookies(origin)
	if len(cookies) != 1 || cookies[0].Name != "sid" || cookies[0].Value != "cookie-value" {
		t.Fatalf("reloaded cookies = %#v, want one sid=cookie-value", cookies)
	}
}

// TestSessionFileIsPrivate pins the on-disk permissions: the cookie file is
// owner-only and its directory is private, so a session is never left where a
// group or another local account could read it (spec §22, §23, §40).
func TestSessionFileIsPrivate(t *testing.T) {
	// The session directory is created lazily beneath the Relay home, so point
	// the store at a not-yet-existing child and check the mode it is born with.
	dir := filepath.Join(t.TempDir(), sessionsDirName)
	origin := mustURL(t, "https://api.example.com/")

	store := NewStore(dir)
	session, err := store.Jar("demo")
	if err != nil {
		t.Fatalf("Jar: %v", err)
	}
	session.SetCookies(origin, []*http.Cookie{{Name: "sid", Value: "cookie-value", Path: "/"}})
	if err := session.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fileInfo, err := os.Stat(filepath.Join(dir, "demo.json"))
	if err != nil {
		t.Fatalf("stat session file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != sessionFileMode {
		t.Errorf("session file mode = %o, want %o", got, sessionFileMode)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat session dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("session dir mode = %o, want 700", got)
	}
}

// TestStoreClearRemovesSession proves clearing is explicit and leaves nothing
// behind, so the next operation sees the same AUTH_REQUIRED a never-logged-in
// tool would (spec §23).
func TestStoreClearRemovesSession(t *testing.T) {
	dir := t.TempDir()
	origin := mustURL(t, "https://api.example.com/")
	store := NewStore(dir)

	session, err := store.Jar("demo")
	if err != nil {
		t.Fatalf("Jar: %v", err)
	}
	session.SetCookies(origin, []*http.Cookie{{Name: "sid", Value: "cookie-value", Path: "/"}})
	if err := session.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := store.Clear("demo"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo.json")); !os.IsNotExist(err) {
		t.Fatalf("session file still present after Clear: %v", err)
	}
	has, err := store.Has("demo")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if has {
		t.Fatal("cleared session should report no cookies")
	}
	// Clearing is idempotent.
	if err := store.Clear("demo"); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
}

// TestStoreRejectsUnsafeToolName keeps a manifest-supplied name from escaping
// the session directory (spec §15).
func TestStoreRejectsUnsafeToolName(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Jar("../escape"); err == nil {
		t.Fatal("Jar should reject a tool name containing a path separator")
	}
	if err := store.Clear("../escape"); err == nil {
		t.Fatal("Clear should reject a tool name containing a path separator")
	}
}
