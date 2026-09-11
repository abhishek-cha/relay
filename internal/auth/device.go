package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"relay/internal/keychain"
	"relay/pkg/relay"
)

// OAuth2 device authorization grant (RFC 8628) — spec §21, §54.
//
// The device grant lets a tool obtain an access token when it has no browser
// and no redirect URI: the daemon asks the authorization server for a short
// user code, the human types it at a verification URI, and the daemon polls the
// token endpoint until the user finishes. The human never pastes a token, and
// the daemon never hands one back: the device code authorizes the token request
// (RFC 8628 §5.2), so it is a secret too.
//
// Nothing in this file returns a device code or an access token to a caller
// that would display it, and no error embeds either (spec §22, §54).

const (
	// DeviceGrantType is the grant type the token endpoint expects for a device
	// authorization grant (RFC 8628 §3.4).
	DeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// The RFC 8628 §3.5 error codes the token endpoint returns while the user
	// has not finished authorizing. They drive the polling loop rather than
	// reaching the caller as failures.
	deviceErrorPending  = "authorization_pending"
	deviceErrorSlowDown = "slow_down"
	deviceErrorDenied   = "access_denied"
	deviceErrorExpired  = "expired_token"

	// defaultDeviceInterval is the polling interval to use when the
	// authorization server omits one (RFC 8628 §3.5).
	defaultDeviceInterval = 5 * time.Second
	// deviceIntervalFloor is the shortest interval this client will poll at,
	// even if a server asks for less. RFC 8628 §3.5 tells clients never to
	// hammer the token endpoint.
	deviceIntervalFloor = 5 * time.Second
	// slowDownIncrement is how much RFC 8628 §3.5 requires the interval to grow
	// by when the server answers slow_down.
	slowDownIncrement = 5 * time.Second

	// deviceResponseLimit bounds an authorization-server response. The server
	// is remote and therefore untrusted: an unbounded body is a denial of
	// service on the daemon, not a curiosity (spec §40).
	deviceResponseLimit = 1 << 20
)

// DeviceFlowSpec is the OAuth2 device authorization grant configuration for
// one tool (spec §21, §54).
//
// The endpoints and client id come from the tool's manifest, which is machine
// truth for what the tool needs (spec §3). Tool is daemon-supplied context: it
// is never parsed from a manifest and exists only so an error can name the tool
// the human is trying to log in to.
//
// There is deliberately no client secret. A device grant is a public-client
// flow, and the only place a manifest-declared client secret could live is the
// tool binary itself — which is not an acceptable secret store.
type DeviceFlowSpec struct {
	Tool                        string
	DeviceAuthorizationEndpoint string
	TokenEndpoint               string
	ClientID                    string
	Scopes                      []string
}

// validate reports the manifest fields a device flow cannot run without. The
// message names fields, never values, so a malformed manifest is diagnosable
// without echoing what it contains.
func (s DeviceFlowSpec) validate() *relay.Error {
	var missing []string
	if strings.TrimSpace(s.DeviceAuthorizationEndpoint) == "" {
		missing = append(missing, "auth.deviceAuthorizationEndpoint")
	}
	if strings.TrimSpace(s.TokenEndpoint) == "" {
		missing = append(missing, "auth.tokenEndpoint")
	}
	if strings.TrimSpace(s.ClientID) == "" {
		missing = append(missing, "auth.clientId")
	}
	if len(missing) == 0 {
		return nil
	}
	return relay.NewError(relay.CodeInvalidInput,
		fmt.Sprintf("the manifest declares an OAuth2 device flow but is missing %s",
			strings.Join(missing, ", ")))
}

// DeviceAuthorization is the authorization server's device_authorization
// response (RFC 8628 §3.2).
//
// DeviceCode is a secret: possession of it is what authorizes the token
// request, so it stays in the daemon (spec §22). UserCode, the verification
// URIs, and the expiry are the user-facing half and are the only parts the CLI
// is allowed to see.
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
	Interval                time.Duration
}

// Token is a successful access-token response (RFC 6749 §5.1). Both token
// values are secrets and are never returned to a caller that displays them
// (spec §22, §54).
type Token struct {
	AccessToken  string
	TokenType    string
	Scope        string
	ExpiresIn    time.Duration
	RefreshToken string
}

// DeviceOptions configures a DeviceClient. A zero value gives the RFC 8628
// defaults over a bounded HTTP client; Now and Sleep exist so tests can drive
// the polling clock instead of waiting on real time.
type DeviceOptions struct {
	HTTP  *http.Client
	Now   func() time.Time
	Sleep func(ctx context.Context, wait time.Duration) error
}

// DeviceClient performs the two HTTP legs of the device grant. It is stateless
// apart from its timing policy: pending authorizations are the daemon's to
// hold, because it is the daemon that owns credentials (spec §21, §22).
type DeviceClient struct {
	http        *http.Client
	now         func() time.Time
	sleep       func(ctx context.Context, wait time.Duration) error
	minInterval time.Duration
}

// NewDeviceClient returns a device client. Passing a nil HTTP client selects a
// bounded default so one unresponsive authorization server cannot pin the
// daemon forever.
func NewDeviceClient(options DeviceOptions) *DeviceClient {
	client := options.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	return &DeviceClient{http: client, now: now, sleep: sleep, minInterval: deviceIntervalFloor}
}

// Start begins a device authorization and returns the user-facing half
// (RFC 8628 §3.1, §3.2). The device code it also returns is for the daemon to
// hold, never to display.
func (c *DeviceClient) Start(ctx context.Context, spec DeviceFlowSpec) (DeviceAuthorization, *relay.Error) {
	if failure := spec.validate(); failure != nil {
		return DeviceAuthorization{}, failure
	}

	form := url.Values{}
	form.Set("client_id", spec.ClientID)
	if len(spec.Scopes) > 0 {
		form.Set("scope", strings.Join(spec.Scopes, " "))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		spec.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceAuthorization{}, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("cannot build the device authorization request: %v", err))
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return DeviceAuthorization{}, relay.NewError(relay.CodeTimeout,
				"the device authorization request was cancelled")
		}
		return DeviceAuthorization{}, relay.NewError(relay.CodeNetworkError,
			fmt.Sprintf("could not reach the device authorization endpoint: %v", err))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, deviceResponseLimit))
	if err != nil {
		return DeviceAuthorization{}, relay.NewError(relay.CodeNetworkError,
			"could not read the device authorization response")
	}

	var payload struct {
		DeviceCode              string          `json:"device_code"`
		UserCode                string          `json:"user_code"`
		VerificationURI         string          `json:"verification_uri"`
		VerificationURIComplete string          `json:"verification_uri_complete"`
		ExpiresIn               json.RawMessage `json:"expires_in"`
		Interval                json.RawMessage `json:"interval"`
		Error                   string          `json:"error"`
		ErrorDescription        string          `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// The body is not echoed: a remote server could reflect a secret back.
		return DeviceAuthorization{}, relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the device authorization endpoint returned a response this build cannot read (HTTP %d)",
				response.StatusCode))
	}
	// A 5xx is the authorization server failing, not a verdict on the request:
	// report it as a remote error before reading any error body, matching how
	// the rest of the daemon maps upstream status codes (spec §40).
	if response.StatusCode >= 500 {
		return DeviceAuthorization{}, relay.NewError(relay.CodeRemoteError,
			fmt.Sprintf("the device authorization endpoint failed with HTTP %d", response.StatusCode))
	}
	if payload.Error != "" {
		return DeviceAuthorization{}, deviceFailed(spec.Tool, payload.Error, scrub(payload.ErrorDescription))
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return DeviceAuthorization{}, relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the device authorization endpoint refused the request (HTTP %d)", response.StatusCode))
	}
	if payload.DeviceCode == "" || payload.UserCode == "" || payload.VerificationURI == "" {
		return DeviceAuthorization{}, relay.NewError(relay.CodeProtocolError,
			"the device authorization response is missing device_code, user_code, or verification_uri")
	}
	expiresIn, ok := seconds(payload.ExpiresIn)
	if !ok {
		// RFC 8628 §3.2 makes expires_in REQUIRED; without it a pending flow
		// would never expire, so refuse rather than poll forever.
		return DeviceAuthorization{}, relay.NewError(relay.CodeProtocolError,
			"the device authorization response has no usable expires_in")
	}
	interval, ok := seconds(payload.Interval)
	if !ok || interval < c.minInterval {
		interval = c.minInterval
	}

	return DeviceAuthorization{
		DeviceCode:              payload.DeviceCode,
		UserCode:                payload.UserCode,
		VerificationURI:         payload.VerificationURI,
		VerificationURIComplete: payload.VerificationURIComplete,
		ExpiresIn:               expiresIn,
		Interval:                interval,
	}, nil
}

// Wait polls the token endpoint until the user finishes authorizing, the code
// expires, or the request is denied (RFC 8628 §3.4, §3.5).
//
// It honours the server's interval, adds slowDownIncrement whenever the server
// answers slow_down, and gives up once the authorization's own expiry has
// passed. The returned token never appears in an error.
func (c *DeviceClient) Wait(ctx context.Context, spec DeviceFlowSpec, authorization DeviceAuthorization) (*Token, *relay.Error) {
	if strings.TrimSpace(authorization.DeviceCode) == "" {
		return nil, relay.NewError(relay.CodeInvalidInput, "the device authorization has no device code")
	}
	interval := authorization.Interval
	if interval < c.minInterval {
		interval = c.minInterval
	}
	deadline := c.now().Add(authorization.ExpiresIn)

	for {
		remaining := deadline.Sub(c.now())
		if remaining <= 0 {
			return nil, deviceExpired(spec.Tool)
		}
		wait := interval
		if wait > remaining {
			wait = remaining
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, relay.NewError(relay.CodeTimeout,
				"the device login was cancelled before the user finished authorizing")
		}

		token, oauthError, description, failure := c.requestToken(ctx, spec, authorization.DeviceCode)
		if failure != nil {
			return nil, failure
		}
		if token != nil {
			return token, nil
		}
		switch oauthError {
		case deviceErrorPending:
			continue
		case deviceErrorSlowDown:
			// The server is asking us to back off; RFC 8628 §3.5 wants exactly
			// this increment, and it stays for the rest of the flow.
			interval += slowDownIncrement
			continue
		case deviceErrorExpired:
			return nil, deviceExpired(spec.Tool)
		case deviceErrorDenied:
			return nil, relay.NewError(relay.CodeAuthFailed,
				fmt.Sprintf("the authorization request for %s was denied", toolLabel(spec.Tool)))
		default:
			return nil, deviceFailed(spec.Tool, oauthError, description)
		}
	}
}

// requestToken performs one token-endpoint exchange.
//
// Exactly one of token, the OAuth2 error pair, and failure is meaningful: a
// token on success, an OAuth2 error code for a well-formed rejection the caller
// may keep polling on, or a failure for anything the caller cannot act on.
func (c *DeviceClient) requestToken(ctx context.Context, spec DeviceFlowSpec, deviceCode string) (*Token, string, string, *relay.Error) {
	form := url.Values{}
	form.Set("grant_type", DeviceGrantType)
	form.Set("device_code", deviceCode)
	form.Set("client_id", spec.ClientID)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		spec.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", "", relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("cannot build the token request: %v", err))
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", "", relay.NewError(relay.CodeTimeout,
				"the device login was cancelled before the user finished authorizing")
		}
		return nil, "", "", relay.NewError(relay.CodeNetworkError,
			scrub(fmt.Sprintf("the token request could not reach the authorization server: %v", err), deviceCode))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, deviceResponseLimit))
	if err != nil {
		return nil, "", "", relay.NewError(relay.CodeNetworkError, "could not read the token response")
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
		return nil, "", "", relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the token endpoint returned a response this build cannot read (HTTP %d)",
				response.StatusCode))
	}
	if response.StatusCode >= 500 {
		return nil, "", "", relay.NewError(relay.CodeRemoteError,
			fmt.Sprintf("the token endpoint failed with HTTP %d", response.StatusCode))
	}
	if payload.Error != "" {
		return nil, payload.Error, scrub(payload.ErrorDescription, deviceCode), nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, "", "", relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the token endpoint refused the token request (HTTP %d)", response.StatusCode))
	}
	if payload.AccessToken == "" {
		return nil, "", "", relay.NewError(relay.CodeProtocolError,
			"the token endpoint returned no access token")
	}
	expiresIn, _ := seconds(payload.ExpiresIn)
	return &Token{
		AccessToken:  payload.AccessToken,
		TokenType:    payload.TokenType,
		Scope:        payload.Scope,
		ExpiresIn:    expiresIn,
		RefreshToken: payload.RefreshToken,
	}, "", "", nil
}

// deviceExpired is the terminal "the code timed out" error. It names the tool
// and the remedy and carries no secret.
func deviceExpired(tool string) *relay.Error {
	return relay.NewError(relay.CodeAuthFailed,
		fmt.Sprintf("the device login for %s expired before it was approved; start it again to get a new code",
			toolLabel(tool)))
}

// deviceFailed is the terminal "the server rejected the request" error. The
// OAuth2 code is a symbolic name from RFC 6749; the description is remote text
// that has already been scrubbed of everything the flow knows is secret.
func deviceFailed(tool, oauthError, description string) *relay.Error {
	message := fmt.Sprintf("the authorization server rejected the device login for %s (%s)",
		toolLabel(tool), oauthError)
	if description != "" {
		message += ": " + description
	}
	return relay.NewError(relay.CodeAuthFailed, message)
}

// toolLabel keeps a message readable when the daemon supplied no tool name.
func toolLabel(tool string) string {
	if strings.TrimSpace(tool) == "" {
		return "this tool"
	}
	return tool
}

// scrub removes every secret the flow knows from remote-supplied text before
// it can reach a log, an error, or the CLI (spec §22, §54).
func scrub(message string, secrets ...string) string {
	return keychain.Redact(message, secrets...)
}

// seconds decodes an OAuth2 duration field. RFC 8628 specifies a JSON number,
// but real servers also send numeric strings, so both are accepted.
func seconds(raw json.RawMessage) (time.Duration, bool) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return 0, false
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return time.Duration(value * float64(time.Second)), true
}

// sleepContext waits for a duration or until the context ends.
func sleepContext(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
