package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"relay/pkg/relay"
)

// OAuth2 browser login: the authorization code grant with PKCE (RFC 6749 §4.1,
// RFC 7636) — spec §21, §23, §54.
//
// The browser grant is for a tool whose authorization server offers a redirect:
// the daemon binds a loopback listener, hands the human a URL, and the browser
// comes back with an authorization code. PKCE replaces a client secret, because
// this is a public client and the only place a manifest-declared secret could
// live is the tool binary, which is not a secret store.
//
// The state and the PKCE challenge are public by design: they travel in the URL
// the human clicks. The PKCE verifier is the secret half, and it goes to exactly
// one place — the token endpoint — and never into an error, a log, or the
// authorization URL. No error here returns a code, a verifier, or a response
// body (spec §22, §40, §54).

const (
	// browserResponseLimit bounds an authorization-server token response. The
	// server is remote and therefore untrusted, so an unbounded body is a
	// denial of service on the daemon, not a curiosity (spec §40). It mirrors
	// deviceResponseLimit.
	browserResponseLimit = deviceResponseLimit

	// browserRandBytes is the entropy behind the state and the PKCE verifier.
	// 32 bytes of base64url is 43 characters, the shortest verifier RFC 7636
	// §4.1 allows and comfortably inside the 43-128 character range.
	browserRandBytes = 32

	// defaultCallbackPath is the loopback path used when a manifest declares a
	// redirect URI without one. Relay picks it, so Begin always knows the exact
	// path Wait must require.
	defaultCallbackPath = "/callback"
)

// BrowserFlowSpec is the OAuth2 authorization-code-with-PKCE configuration for
// one tool (spec §21, §23, §54).
//
// The endpoints and client id come from the tool's manifest, which is machine
// truth for what the tool needs (spec §3). Tool is daemon-supplied context: it
// is never parsed from a manifest and exists only so an error can name the tool
// the human is trying to log in to.
//
// RedirectURI is optional; empty means Relay picks an ephemeral 127.0.0.1 port
// itself. There is deliberately no client secret: PKCE replaces it, and the only
// place a manifest-declared secret could live is the tool binary, which is not a
// secret store.
type BrowserFlowSpec struct {
	Tool                  string
	AuthorizationEndpoint string
	TokenEndpoint         string
	ClientID              string
	Scopes                []string
	RedirectURI           string
}

// validate reports the fields a browser flow cannot run without, and checks a
// declared redirect URI for the shape a loopback callback must have. Messages
// name fields, never values, so a malformed manifest is diagnosable without
// echoing what it contains.
func (s BrowserFlowSpec) validate() *relay.Error {
	var missing []string
	if strings.TrimSpace(s.AuthorizationEndpoint) == "" {
		missing = append(missing, "auth.authorizationEndpoint")
	}
	if strings.TrimSpace(s.TokenEndpoint) == "" {
		missing = append(missing, "auth.tokenEndpoint")
	}
	if strings.TrimSpace(s.ClientID) == "" {
		missing = append(missing, "auth.clientId")
	}
	if len(missing) > 0 {
		return relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("the manifest declares an OAuth2 browser flow but is missing %s",
				strings.Join(missing, ", ")))
	}
	return validateLoopbackRedirect(s.RedirectURI)
}

// validateLoopbackRedirect enforces that a declared redirect URI is an http
// loopback URL: host 127.0.0.1 or localhost, an optional port, a path, and no
// query or fragment. Anything else could send the authorization code somewhere
// the daemon is not listening.
func validateLoopbackRedirect(raw string) *relay.Error {
	declared := strings.TrimSpace(raw)
	if declared == "" {
		return nil
	}
	invalid := relay.NewError(relay.CodeInvalidInput,
		"the manifest's auth.redirectURI must be an http loopback URL on 127.0.0.1 or localhost with a path and no query or fragment")

	parsed, err := url.Parse(declared)
	if err != nil {
		return invalid
	}
	if parsed.Scheme != "http" || parsed.Host == "" {
		return invalid
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" {
		return invalid
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return invalid
	}
	if parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") {
		return invalid
	}
	return nil
}

// BrowserOptions configures a BrowserClient. Every field is a seam: tests
// inject a listener and a deterministic Rand instead of binding a fixed port
// and waiting on real time. A zero value gives a bounded HTTP client, an
// ephemeral loopback listener, the system clock, and crypto/rand.
type BrowserOptions struct {
	HTTP     *http.Client
	Listener func(spec BrowserFlowSpec) (net.Listener, error)
	Now      func() time.Time
	Rand     io.Reader
}

// BrowserClient starts browser logins. It is stateless; each Begin returns an
// independent flow that owns its listener.
type BrowserClient struct {
	http     *http.Client
	listener func(BrowserFlowSpec) (net.Listener, error)
	now      func() time.Time
	rand     io.Reader
}

// NewBrowserClient returns a browser client. Passing a nil HTTP client selects
// a bounded default so one unresponsive authorization server cannot pin the
// daemon forever.
func NewBrowserClient(options BrowserOptions) *BrowserClient {
	client := options.HTTP
	if client == nil {
		client = &http.Client{Timeout: 300 * time.Second}
	}
	listener := options.Listener
	if listener == nil {
		listener = defaultLoopbackListener
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	source := options.Rand
	if source == nil {
		source = rand.Reader
	}
	return &BrowserClient{http: client, listener: listener, now: now, rand: source}
}

// Begin binds the loopback listener first, so the redirect URI names the port
// that actually got bound, then mints an unguessable state and a fresh PKCE
// S256 verifier. The caller reads URL() and hands it to the human, then calls
// Wait().
func (c *BrowserClient) Begin(ctx context.Context, spec BrowserFlowSpec) (*BrowserFlow, *relay.Error) {
	if failure := spec.validate(); failure != nil {
		return nil, failure
	}
	listener, err := c.listener(spec)
	if err != nil {
		// The value is not echoed; only that the loopback listener could not
		// be bound.
		return nil, relay.NewError(relay.CodeAuthFailed,
			"could not bind the loopback callback listener for the browser login")
	}
	redirectURI, callbackPath, failure := redirectFor(listener, spec)
	if failure != nil {
		_ = listener.Close()
		return nil, failure
	}
	state, failure := randomURLSafe(c.rand, browserRandBytes)
	if failure != nil {
		_ = listener.Close()
		return nil, failure
	}
	verifier, failure := randomURLSafe(c.rand, browserRandBytes)
	if failure != nil {
		_ = listener.Close()
		return nil, failure
	}
	challenge := codeChallenge(verifier)

	return &BrowserFlow{
		spec:         spec,
		http:         c.http,
		now:          c.now,
		listener:     listener,
		redirectURI:  redirectURI,
		callbackPath: callbackPath,
		state:        state,
		verifier:     verifier,
		challenge:    challenge,
	}, nil
}

// BrowserFlow is one in-flight browser login. It owns its loopback listener
// and the one-shot secrets — state and the PKCE verifier — that must not be
// shown to the human. URL() and RedirectURI() are the only safe outputs.
type BrowserFlow struct {
	spec         BrowserFlowSpec
	http         *http.Client
	now          func() time.Time
	listener     net.Listener
	redirectURI  string
	callbackPath string
	state        string
	verifier     string
	challenge    string

	closeOnce sync.Once
	closeErr  error
	waitOnce  sync.Once
	token     *Token
	failure   *relay.Error
}

// URL is the authorization URL to hand the human. It carries the state and the
// PKCE challenge, both public by design, and never the verifier or any token.
func (f *BrowserFlow) URL() string {
	parsed, err := url.Parse(f.spec.AuthorizationEndpoint)
	if err != nil {
		// Begin validated the endpoint is non-empty; a parse failure here is
		// remote configuration, and the value is not echoed.
		return f.spec.AuthorizationEndpoint
	}
	query := parsed.Query()
	query.Set("response_type", "code")
	query.Set("client_id", f.spec.ClientID)
	query.Set("redirect_uri", f.redirectURI)
	query.Set("state", f.state)
	query.Set("code_challenge", f.challenge)
	query.Set("code_challenge_method", "S256")
	if len(f.spec.Scopes) > 0 {
		query.Set("scope", strings.Join(f.spec.Scopes, " "))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// RedirectURI is the loopback callback URL the authorization server will send
// the browser back to. It names the port Begin actually bound.
func (f *BrowserFlow) RedirectURI() string {
	return f.redirectURI
}

// Wait serves the loopback callback and exchanges the authorization code.
//
// It accepts connections until one carries a code or a server error, tolerating
// extra requests (a browser routinely asks for /favicon.ico) and requiring the
// exact callback path. It verifies state with a constant-time comparison before
// doing anything with the code, because a mismatched state means the request
// was not ours and nothing may be exchanged.
//
// Wait is safe to call once: the listener is released and the one-shot secrets
// are drained when it returns, after which Close is a no-op.
func (f *BrowserFlow) Wait(ctx context.Context) (*Token, *relay.Error) {
	f.waitOnce.Do(func() {
		f.token, f.failure = f.serve(ctx)
		_ = f.release()
	})
	return f.token, f.failure
}

// serve is Wait's body: it owns the accept loop and returns the one result.
func (f *BrowserFlow) serve(ctx context.Context) (*Token, *relay.Error) {
	if f.listener == nil {
		return nil, relay.NewError(relay.CodeAuthFailed, "the browser login has no loopback listener")
	}
	// Unblock Accept when the caller's context ends, so cancellation is prompt
	// rather than waiting for a connection that will never arrive.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = f.release()
		case <-stop:
		}
	}()

	for {
		connection, err := f.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, relay.NewError(relay.CodeTimeout, "the browser login was cancelled")
			}
			return nil, relay.NewError(relay.CodeAuthFailed,
				"the loopback callback listener stopped before the login finished")
		}
		request, readErr := http.ReadRequest(bufio.NewReader(connection))
		if readErr != nil {
			writeCallbackResponse(connection, http.StatusBadRequest, "bad request")
			_ = connection.Close()
			continue
		}
		if request.URL.Path != f.callbackPath {
			// Not our callback (e.g. /favicon.ico). Answer and keep listening.
			writeCallbackResponse(connection, http.StatusNotFound, "not found")
			_ = connection.Close()
			continue
		}
		query := request.URL.Query()
		if !constantTimeEqual(query.Get("state"), f.state) {
			writeCallbackResponse(connection, http.StatusBadRequest, "state mismatch")
			_ = connection.Close()
			return nil, relay.NewError(relay.CodeAuthFailed,
				"the browser callback did not match this login; the request was not ours")
		}
		if oauthError := query.Get("error"); oauthError != "" {
			writeCallbackResponse(connection, http.StatusBadRequest, "authorization failed")
			_ = connection.Close()
			// The query string is not echoed; only the symbolic OAuth2 error
			// and a scrubbed description.
			return nil, browserFailed(f.spec.Tool, oauthError,
				scrub(query.Get("error_description"), f.verifier, f.state))
		}
		code := query.Get("code")
		if code == "" {
			writeCallbackResponse(connection, http.StatusBadRequest, "missing code")
			_ = connection.Close()
			continue
		}
		writeCallbackResponse(connection, http.StatusOK, "You can close this window and return to Relay.")
		_ = connection.Close()
		return f.exchange(ctx, code)
	}
}

// exchange performs the token-endpoint leg (RFC 6749 §4.1.3, RFC 7636 §4.5).
// The PKCE verifier is sent here and nowhere else; a failure never echoes the
// code, the verifier, or the response body.
func (f *BrowserFlow) exchange(ctx context.Context, code string) (*Token, *relay.Error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", f.redirectURI)
	form.Set("client_id", f.spec.ClientID)
	form.Set("code_verifier", f.verifier)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.spec.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("cannot build the token request: %v", err))
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := f.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, relay.NewError(relay.CodeTimeout, "the browser login was cancelled")
		}
		return nil, relay.NewError(relay.CodeNetworkError,
			scrub(fmt.Sprintf("the token request could not reach the authorization server: %v", err),
				code, f.verifier))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, browserResponseLimit))
	if err != nil {
		return nil, relay.NewError(relay.CodeNetworkError, "could not read the token response")
	}

	var payload struct {
		AccessToken      string          `json:"access_token"`
		TokenType        string          `json:"token_type"`
		Scope            string          `json:"scope"`
		RefreshToken     string          `json:"refresh_token"`
		ExpiresIn        json.RawMessage `json:"expires_in"`
		Error            string          `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the token endpoint returned a response this build cannot read (HTTP %d)",
				response.StatusCode))
	}
	if response.StatusCode >= 500 {
		return nil, relay.NewError(relay.CodeRemoteError,
			fmt.Sprintf("the token endpoint failed with HTTP %d", response.StatusCode))
	}
	if payload.Error != "" {
		return nil, browserFailed(f.spec.Tool, payload.Error,
			scrub(payload.ErrorDescription, code, f.verifier, f.state))
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the token endpoint refused the token request (HTTP %d)", response.StatusCode))
	}
	if payload.AccessToken == "" {
		return nil, relay.NewError(relay.CodeProtocolError,
			"the token endpoint returned no access token")
	}
	expiresIn, _ := seconds(payload.ExpiresIn)
	return &Token{
		AccessToken:  payload.AccessToken,
		TokenType:    payload.TokenType,
		Scope:        payload.Scope,
		ExpiresIn:    expiresIn,
		RefreshToken: payload.RefreshToken,
	}, nil
}

// Close releases the loopback listener. It is safe to call twice, and a no-op
// after Wait has returned.
func (f *BrowserFlow) Close() error {
	return f.release()
}

// release closes the listener at most once and remembers the outcome, so Wait
// and Close cannot close it twice.
func (f *BrowserFlow) release() error {
	f.closeOnce.Do(func() {
		if f.listener != nil {
			f.closeErr = f.listener.Close()
		}
	})
	return f.closeErr
}

// browserFailed is the terminal "the server rejected the browser login" error.
// The OAuth2 code is a symbolic name from RFC 6749; the description is remote
// text already scrubbed of everything the flow knows is secret.
func browserFailed(tool, oauthError, description string) *relay.Error {
	message := fmt.Sprintf("the authorization server rejected the browser login for %s (%s)",
		toolLabel(tool), oauthError)
	if description != "" {
		message += ": " + description
	}
	return relay.NewError(relay.CodeAuthFailed, message)
}

// defaultLoopbackListener binds the loopback address the redirect URI names. A
// declared port is honoured so the URL a human clicks keeps working; otherwise
// an ephemeral port is chosen.
func defaultLoopbackListener(spec BrowserFlowSpec) (net.Listener, error) {
	host := "127.0.0.1"
	port := "0"
	if declared := strings.TrimSpace(spec.RedirectURI); declared != "" {
		if parsed, err := url.Parse(declared); err == nil {
			if name := parsed.Hostname(); name != "" {
				host = name
			}
			if explicit := parsed.Port(); explicit != "" {
				port = explicit
			}
		}
	}
	return net.Listen("tcp", net.JoinHostPort(host, port))
}

// redirectFor computes the exact redirect URI and callback path from the bound
// listener and any declared redirect URI. The port always comes from the
// listener, so the URL names the port that actually got bound.
func redirectFor(listener net.Listener, spec BrowserFlowSpec) (string, string, *relay.Error) {
	host := "127.0.0.1"
	path := defaultCallbackPath
	if declared := strings.TrimSpace(spec.RedirectURI); declared != "" {
		parsed, err := url.Parse(declared)
		if err != nil {
			return "", "", relay.NewError(relay.CodeInvalidInput,
				"the manifest's auth.redirectURI is not a URL this build can read")
		}
		if name := parsed.Hostname(); name != "" {
			host = name
		}
		if parsed.Path != "" {
			path = parsed.Path
		}
	}
	port := listenerPort(listener)
	return "http://" + net.JoinHostPort(host, port) + path, path, nil
}

// listenerPort reads the bound port off a listener, tolerating any net.Listener
// that is not a *net.TCPAddr.
func listenerPort(listener net.Listener) string {
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
		return strconv.Itoa(tcp.Port)
	}
	if _, port, err := net.SplitHostPort(listener.Addr().String()); err == nil {
		return port
	}
	return "0"
}

// randomURLSafe returns size random bytes as unpadded base64url, the alphabet
// RFC 7636 §4.1 requires for a verifier.
func randomURLSafe(source io.Reader, size int) (string, *relay.Error) {
	buffer := make([]byte, size)
	if _, err := io.ReadFull(source, buffer); err != nil {
		return "", relay.NewError(relay.CodeRuntimeIncompatible,
			"could not read randomness for the browser login")
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// codeChallenge is the S256 transformation of the verifier (RFC 7636 §4.2).
// PKCE is always S256 here; plain is never used.
func codeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// constantTimeEqual compares two strings without leaking where they differ.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// writeCallbackResponse answers the browser with a minimal HTTP response. The
// body never contains a token or any part of the query string.
func writeCallbackResponse(connection net.Conn, status int, body string) {
	response := &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	_ = response.Write(connection)
}
