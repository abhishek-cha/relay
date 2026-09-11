package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"relay/internal/fsutil"
	"relay/internal/paths"
	"relay/internal/registry"
)

// Session state layout under the Relay home.
//
// A browser tool's cookies live in one JSON file per tool, beside the registry
// rather than inside it, so discovery metadata and a session never share a
// file. Everything here is daemon-owned: the session is read only by the
// executor that reuses it and never crosses IPC, lands in a log, or appears in
// telemetry (spec §22, §23, §40).
const (
	// sessionsDirName is the per-Relay-home directory holding session files.
	sessionsDirName = "sessions"
	// sessionFileMode keeps a session cookie readable only by its owner.
	sessionFileMode = 0o600
	// sessionDirMode keeps the session directory private even before the first
	// file exists.
	sessionDirMode = 0o700
)

// SessionDir returns the daemon's session directory for a Relay home. The
// daemon passes it to [New]; tests point it at a temporary directory.
func SessionDir(layout paths.Layout) string {
	return filepath.Join(layout.Root, sessionsDirName)
}

// Session is one tool's cookie state: a live [http.CookieJar] the HTTP client
// consults, plus the persistence the daemon owns.
//
// The cookie values are secrets. Nothing in this package ever formats a
// [*http.Cookie] into an error, a log, or an IPC reply (spec §22).
type Session interface {
	http.CookieJar

	// HasCookies reports whether any stored cookie is still live. It is the
	// session-presence check an operation makes before contacting a service.
	HasCookies() bool

	// Save atomically writes the session to disk. It is safe to call after any
	// exchange that may have rotated a cookie.
	Save() error
}

// Store is a directory of per-tool sessions, mirroring the registry's one-file-
// per-tool layout (spec §15, §23). It caches loaded jars so a session survives
// across invocations of the daemon process without re-reading the file.
type Store struct {
	dir  string
	mu   sync.Mutex
	jars map[string]*sessionJar
}

// NewStore returns a store rooted at dir. The directory is created lazily, with
// [sessionDirMode], so an unwritable home fails at first use rather than here.
func NewStore(dir string) *Store {
	return &Store{dir: dir, jars: map[string]*sessionJar{}}
}

// Dir is the store's directory.
func (s *Store) Dir() string { return s.dir }

// Jar returns the tool's session, loading it from disk on first use. A tool
// with no session file yields an empty session rather than an error: "not
// logged in yet" is a normal state the caller reports as AUTH_REQUIRED.
func (s *Store) Jar(tool string) (Session, error) {
	if !registry.ValidName(tool) {
		return nil, fmt.Errorf("invalid tool name %q", tool)
	}
	s.mu.Lock()
	cached, ok := s.jars[tool]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}

	loaded, err := loadSession(s.dir, tool)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if existing, ok := s.jars[tool]; ok {
		loaded = existing
	} else {
		s.jars[tool] = loaded
	}
	s.mu.Unlock()
	return loaded, nil
}

// Has reports whether the tool has a live session.
func (s *Store) Has(tool string) (bool, error) {
	session, err := s.Jar(tool)
	if err != nil {
		return false, err
	}
	return session.HasCookies(), nil
}

// Clear removes a tool's session. It is idempotent: clearing a tool with no
// session is reported as cleared, because that is the state the caller asked
// for.
func (s *Store) Clear(tool string) error {
	if !registry.ValidName(tool) {
		return fmt.Errorf("invalid tool name %q", tool)
	}
	s.mu.Lock()
	delete(s.jars, tool)
	s.mu.Unlock()
	if err := os.Remove(s.path(tool)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session for %s: %w", tool, err)
	}
	return nil
}

// Save persists the tool's currently cached session.
func (s *Store) Save(tool string) error {
	s.mu.Lock()
	session, ok := s.jars[tool]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return session.Save()
}

func (s *Store) path(tool string) string {
	return filepath.Join(s.dir, tool+".json")
}

// sessionJar is the live cookie jar for one tool. It keeps the cookiejar's
// RFC 6265 matching and expiry behaviour and, because net/http/cookiejar
// exposes no way to enumerate what it holds, records every cookie the client
// hands it so the session can be written to and read back from disk.
type sessionJar struct {
	jar  *cookiejar.Jar
	path string

	mu  sync.Mutex
	set []recordedCookie
}

// sessionFile is the on-disk shape of one session.
type sessionFile struct {
	Cookies []recordedCookie `json:"cookies"`
}

// recordedCookie is one cookie plus the origin it was set from, which is what
// lets the jar be rebuilt with the same host-only vs domain-wide decision the
// server's Set-Cookie implied.
type recordedCookie struct {
	Origin   string    `json:"origin"`
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	MaxAge   int       `json:"maxAge,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"httpOnly,omitempty"`
}

func newRecordedCookie(u *url.URL, c *http.Cookie) recordedCookie {
	origin := ""
	if u != nil {
		origin = u.String()
	}
	return recordedCookie{
		Origin:   origin,
		Name:     c.Name,
		Value:    c.Value,
		Path:     c.Path,
		Domain:   c.Domain,
		Expires:  c.Expires,
		MaxAge:   c.MaxAge,
		Secure:   c.Secure,
		HTTPOnly: c.HttpOnly,
	}
}

func (r recordedCookie) cookie() *http.Cookie {
	return &http.Cookie{
		Name:     r.Name,
		Value:    r.Value,
		Path:     r.Path,
		Domain:   r.Domain,
		Expires:  r.Expires,
		MaxAge:   r.MaxAge,
		Secure:   r.Secure,
		HttpOnly: r.HTTPOnly,
	}
}

// key identifies the slot a cookie occupies: a later set with the same key
// replaces an earlier one, and a deletion with the same key removes it.
func (r recordedCookie) key() string {
	return strings.Join([]string{r.Origin, r.Domain, r.Path, r.Name}, "\x00")
}

// dead reports whether the cookie is already expired or was a deletion.
func (r recordedCookie) dead(now time.Time) bool {
	if r.MaxAge < 0 {
		return true
	}
	return !r.Expires.IsZero() && r.Expires.Before(now)
}

func loadSession(dir, tool string) (*sessionJar, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}
	session := &sessionJar{jar: jar, path: filepath.Join(dir, tool+".json")}

	data, err := os.ReadFile(session.path)
	if errors.Is(err, os.ErrNotExist) {
		return session, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session for %s: %w", tool, err)
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("session for %s is unreadable: %w", tool, err)
	}
	now := time.Now()
	for _, recorded := range file.Cookies {
		if recorded.dead(now) {
			continue
		}
		origin, err := url.Parse(recorded.Origin)
		if err != nil {
			// A cookie with no usable origin cannot be placed; dropping it
			// narrows the session rather than widening it.
			continue
		}
		session.jar.SetCookies(origin, []*http.Cookie{recorded.cookie()})
		session.set = append(session.set, recorded)
	}
	return session, nil
}

// SetCookies delegates matching and expiry to the cookiejar and records the
// raw cookies so the session can be persisted.
func (j *sessionJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.jar.SetCookies(u, cookies)
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, cookie := range cookies {
		recorded := newRecordedCookie(u, cookie)
		if recorded.dead(now) {
			j.set = removeCookie(j.set, recorded.key())
			continue
		}
		j.set = upsertCookie(j.set, recorded)
	}
	j.set = compactCookies(j.set, now)
}

// Cookies implements http.CookieJar.
func (j *sessionJar) Cookies(u *url.URL) []*http.Cookie { return j.jar.Cookies(u) }

// HasCookies reports whether any recorded cookie is still live.
func (j *sessionJar) HasCookies() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	for _, recorded := range j.set {
		if !recorded.dead(now) {
			return true
		}
	}
	return false
}

// Save writes the live cookies to disk atomically. When nothing is left to
// store the file is removed, so an empty session never lingers as a file that
// looks like a usable one.
func (j *sessionJar) Save() error {
	j.mu.Lock()
	cookies := compactCookies(j.set, time.Now())
	j.mu.Unlock()

	if len(cookies) == 0 {
		if err := os.Remove(j.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty session: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(j.path), sessionDirMode); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	encoded, err := json.MarshalIndent(sessionFile{Cookies: cookies}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	return fsutil.WriteFile(j.path, append(encoded, '\n'), sessionFileMode)
}

// upsertCookie replaces the cookie with the same key, or appends it.
func upsertCookie(set []recordedCookie, cookie recordedCookie) []recordedCookie {
	for i := range set {
		if set[i].key() == cookie.key() {
			set[i] = cookie
			return set
		}
	}
	return append(set, cookie)
}

// removeCookie drops the cookie with key, if present.
func removeCookie(set []recordedCookie, key string) []recordedCookie {
	for i := range set {
		if set[i].key() == key {
			return append(set[:i], set[i+1:]...)
		}
	}
	return set
}

// compactCookies drops dead cookies and returns a fresh slice, bounding what the
// in-memory record grows to: one entry per distinct cookie slot, not one per
// Set-Cookie header seen.
func compactCookies(set []recordedCookie, now time.Time) []recordedCookie {
	live := make([]recordedCookie, 0, len(set))
	for _, cookie := range set {
		if cookie.dead(now) {
			continue
		}
		live = upsertCookie(live, cookie)
	}
	return live
}
