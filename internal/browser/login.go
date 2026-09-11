package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"relay/internal/keychain"
	"relay/pkg/relay"
)

// maxLoginBodyBytes bounds how much of a login response Relay reads before
// discarding it. A login response is not the operation's payload; the daemon
// only needs the Set-Cookie headers the jar already absorbed, so a hostile
// body must not be buffered without limit (spec §40).
const maxLoginBodyBytes = 1 << 20

// LoginKind selects how a tool proves who it is.
type LoginKind string

const (
	// LoginForm posts the username and password as form fields.
	LoginForm LoginKind = "form"
	// LoginHeader sends the stored secret in a request header.
	LoginHeader LoginKind = "header"
)

// LoginSpec is the login flow a manifest declares under `auth.login`.
//
// The declaration is additive to the existing auth block, exactly like the
// OAuth2 device fields (spec §21, §54): a browser manifest keeps an auth.type
// from the known vocabulary — `basic` for a `user:password` credential, `bearer`
// or `api_key` for a token — and adds the path and field names that turn that
// credential into a session. The schema is owned by internal/manifest;
// [LoginSpecFromManifest] reads the same raw YAML the daemon trusts elsewhere.
//
// OAuth2 `authorization_code` with a redirect is deliberately not implemented.
// It needs a loopback redirect and a browser handoff, which is the honest place
// to say "not yet" rather than half-build a device flow this layer does not own.
type LoginSpec struct {
	Kind    LoginKind
	BaseURL string
	Method  string
	Path    string

	// Header login.
	Header string
	Scheme string

	// Form login.
	UsernameField string
	PasswordField string
}

// manifestLoginBlock mirrors only the manifest fields a browser login needs.
// Unknown fields are ignored by design, so the schema can grow without this
// reader rejecting a newer manifest.
type manifestLoginBlock struct {
	Protocol struct {
		BaseURL  string `yaml:"baseUrl"`
		Endpoint string `yaml:"endpoint"`
	} `yaml:"protocol"`
	Auth *struct {
		Type  string `yaml:"type"`
		Login *struct {
			Kind          string `yaml:"kind"`
			Method        string `yaml:"method"`
			Path          string `yaml:"path"`
			Header        string `yaml:"header"`
			Scheme        string `yaml:"scheme"`
			UsernameField string `yaml:"usernameField"`
			PasswordField string `yaml:"passwordField"`
		} `yaml:"login"`
	} `yaml:"auth"`
}

// LoginSpecFromManifest extracts a browser login declaration from a tool's raw
// manifest.
//
// ok is false when the manifest declares no `auth.login` block, which is a
// tool that needs no session rather than an error. A block that is present but
// incomplete fails loudly: a half-written login would otherwise silently log in
// to the wrong endpoint or with the wrong field name.
func LoginSpecFromManifest(manifestYAML []byte) (spec LoginSpec, ok bool, failure *relay.Error) {
	var document manifestLoginBlock
	if err := yaml.Unmarshal(manifestYAML, &document); err != nil {
		// The manifest body is not echoed: a malformed block could contain a
		// value a human meant to keep out of a diagnostic (spec §22).
		return LoginSpec{}, false, relay.NewError(relay.CodeRuntimeIncompatible,
			"the manifest could not be read for its login block")
	}
	if document.Auth == nil || document.Auth.Login == nil {
		return LoginSpec{}, false, nil
	}
	declared := document.Auth.Login

	baseURL := strings.TrimSpace(document.Protocol.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(document.Protocol.Endpoint)
	}

	method := strings.ToUpper(strings.TrimSpace(declared.Method))
	if method == "" {
		method = http.MethodPost
	}

	spec = LoginSpec{
		Kind:          LoginKind(strings.ToLower(strings.TrimSpace(declared.Kind))),
		BaseURL:       baseURL,
		Method:        method,
		Path:          strings.TrimSpace(declared.Path),
		Header:        strings.TrimSpace(declared.Header),
		Scheme:        strings.TrimSpace(declared.Scheme),
		UsernameField: strings.TrimSpace(declared.UsernameField),
		PasswordField: strings.TrimSpace(declared.PasswordField),
	}

	var missing []string
	if spec.BaseURL == "" {
		missing = append(missing, "protocol.baseUrl (or protocol.endpoint)")
	}
	if spec.Path == "" {
		missing = append(missing, "auth.login.path")
	}
	switch spec.Kind {
	case LoginForm:
		if spec.UsernameField == "" {
			missing = append(missing, "auth.login.usernameField")
		}
		if spec.PasswordField == "" {
			missing = append(missing, "auth.login.passwordField")
		}
	case LoginHeader:
		if spec.Header == "" {
			missing = append(missing, "auth.login.header")
		}
	default:
		return LoginSpec{}, false, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("auth.login.kind must be %q or %q", LoginForm, LoginHeader))
	}
	if len(missing) > 0 {
		return LoginSpec{}, false, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("the manifest declares a login but is missing %s", strings.Join(missing, ", ")))
	}
	return spec, true, nil
}

// Config configures the session layer.
type Config struct {
	// Dir is the daemon's session directory ([SessionDir]).
	Dir string
	// Keychain resolves the login credential. It defaults to the macOS
	// Keychain; tests inject a fake so no real Keychain is touched (spec §40).
	Keychain keychain.Store
	// Transport is the HTTP transport. Nil means http.DefaultTransport.
	Transport http.RoundTripper
	// Timeout bounds one exchange. Zero means [defaultTimeout].
	Timeout time.Duration
}

// Sessions owns the daemon's login flows. It is the narrow seam the daemon
// calls for the `session_login` and `session_clear` IPC frames; the executor
// shares the same [Store] so an operation sees exactly the session a login
// established.
//
// Nothing here returns a cookie, a token, or a secret to its caller. Login
// writes cookies into the store and reports only that it succeeded (spec §22).
type Sessions struct {
	store     *Store
	secrets   keychain.Store
	transport http.RoundTripper
	timeout   time.Duration
}

// New returns a Sessions backed by the directory in cfg.
func New(cfg Config) *Sessions {
	secrets := cfg.Keychain
	if secrets == nil {
		secrets = &keychain.SecurityStore{}
	}
	return &Sessions{
		store:     NewStore(cfg.Dir),
		secrets:   secrets,
		transport: cfg.Transport,
		timeout:   cfg.Timeout,
	}
}

// Store exposes the session store so the executor and tests reuse one cache.
func (s *Sessions) Store() *Store { return s.store }

// HTTPClient builds a session-carrying client with the package's redirect
// policy.
func (s *Sessions) HTTPClient(jar http.CookieJar) *http.Client {
	return HTTPClient(s.transport, s.timeout, jar)
}

// Login runs a manifest's declared login flow, using the tool's stored
// credential, and persists the resulting session.
//
// The credential comes from the Keychain, never from the caller: a login frame
// carries no secret, so there is nothing for the CLI to hold (spec §22). The
// secret is used only to build the login request and never appears in an error,
// a log, or the reply.
func (s *Sessions) Login(ctx context.Context, tool string, manifestYAML []byte) *relay.Error {
	if strings.TrimSpace(tool) == "" {
		return relay.NewError(relay.CodeInvalidInput, "no tool was named")
	}
	spec, ok, failure := LoginSpecFromManifest(manifestYAML)
	if failure != nil {
		return failure
	}
	if !ok {
		return relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q declares no browser login", tool))
	}

	secret, err := s.secrets.Get(keychain.Service(tool), keychain.AccountDefault)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return relay.NewError(relay.CodeAuthRequired,
				fmt.Sprintf("%s has no stored credential for its login; run 'relay auth set %s'", tool, tool))
		}
		// A faulty Store might embed the secret in its error; scrub it anyway.
		return relay.NewError(relay.CodeAuthFailed,
			keychain.Redact(fmt.Sprintf("could not read the stored credential for %q: %v", tool, err), secret))
	}
	if secret == "" {
		return relay.NewError(relay.CodeAuthRequired,
			fmt.Sprintf("%s has no stored credential for its login; run 'relay auth set %s'", tool, tool))
	}

	session, err := s.store.Jar(tool)
	if err != nil {
		return relay.NewError(relay.CodeRemoteError, err.Error())
	}
	request, failure := buildLoginRequest(ctx, spec, secret)
	if failure != nil {
		return failure
	}

	response, err := s.HTTPClient(session).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return relay.NewError(relay.CodeTimeout, "the login request was cancelled or timed out")
		}
		return relay.NewError(relay.CodeNetworkError, "login transport error: "+err.Error())
	}
	defer response.Body.Close()
	// Drain a bounded amount so the connection can be reused; the body itself is
	// intentionally not surfaced, since a login response may carry a token.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxLoginBodyBytes))

	if failure := loginStatusError(response.StatusCode); failure != nil {
		return failure
	}
	// A 2xx that set no cookie did not establish a session. Reporting success
	// here would let the next operation fail as AUTH_REQUIRED with no hint that
	// the credential itself was refused, so the absent session is the failure
	// (spec §23, §26).
	if !session.HasCookies() {
		return relay.NewError(relay.CodeAuthFailed,
			"the login was accepted but set no session cookie; check the declared auth.login fields")
	}
	if err := session.Save(); err != nil {
		return relay.NewError(relay.CodeRemoteError,
			"the login succeeded but its session could not be persisted")
	}
	return nil
}

// Clear removes a tool's session. It clears cookies only: the stored credential
// is a separate concern owned by `relay auth clear`, and clearing a session the
// user still wants would be a surprising way to log them out of the credential
// store too.
func (s *Sessions) Clear(tool string) *relay.Error {
	if strings.TrimSpace(tool) == "" {
		return relay.NewError(relay.CodeInvalidInput, "no tool was named")
	}
	if err := s.store.Clear(tool); err != nil {
		return relay.NewError(relay.CodeRemoteError, err.Error())
	}
	return nil
}

// buildLoginRequest turns a login spec plus the stored secret into the HTTP
// request that establishes the session.
func buildLoginRequest(ctx context.Context, spec LoginSpec, secret string) (*http.Request, *relay.Error) {
	resolved, err := url.Parse(spec.BaseURL + spec.Path)
	if err != nil {
		return nil, relay.NewError(relay.CodeInvalidInput,
			"the manifest's login URL could not be parsed")
	}

	switch spec.Kind {
	case LoginForm:
		username, password, ok := strings.Cut(secret, ":")
		if !ok || username == "" || password == "" {
			return nil, relay.NewError(relay.CodeInvalidInput,
				"form login needs a username:password credential; set one with 'relay auth set'")
		}
		form := url.Values{
			spec.UsernameField: {username},
			spec.PasswordField: {password},
		}
		request, err := http.NewRequestWithContext(ctx, spec.Method, resolved.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return nil, relay.NewError(relay.CodeInvalidInput, "could not build the login request")
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return request, nil

	case LoginHeader:
		request, err := http.NewRequestWithContext(ctx, spec.Method, resolved.String(), nil)
		if err != nil {
			return nil, relay.NewError(relay.CodeInvalidInput, "could not build the login request")
		}
		value := secret
		if spec.Scheme != "" {
			value = spec.Scheme + " " + secret
		}
		request.Header.Set(spec.Header, value)
		return request, nil
	}

	return nil, relay.NewError(relay.CodeInvalidInput, "unknown login kind")
}

// loginStatusError maps a login response status onto the shared taxonomy. The
// body and the credential are deliberately absent from every detail (spec §22).
func loginStatusError(status int) *relay.Error {
	details := map[string]any{"httpStatus": status}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return relay.NewError(relay.CodeAuthFailed,
			"the service rejected the stored credential; replace it with 'relay auth set'").WithDetails(details)
	case status == http.StatusTooManyRequests:
		return relay.NewError(relay.CodeRateLimited, "the login endpoint rate limited the request").WithDetails(details)
	case status >= 500:
		return relay.NewError(relay.CodeRemoteError, fmt.Sprintf("login failed: HTTP %d", status)).WithDetails(details)
	default:
		return relay.NewError(relay.CodeRemoteError, fmt.Sprintf("login failed: HTTP %d", status)).WithDetails(details)
	}
}
