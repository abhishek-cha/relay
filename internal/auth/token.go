package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"relay/pkg/relay"
)

// The token envelope: the single Keychain string that makes background refresh
// possible (spec §21, §22, §54).
//
// The daemon had been storing only the bare access-token string, which threw
// away the refresh token and left an expired token with no way back except
// asking the human to log in again. The envelope keeps the refresh token, the
// expiry, and the endpoint/client context a refresh needs.
//
// The envelope is opt-in and lossless: a stored value that is not this envelope
// is a token a human pasted, and every reader must fall through to using it
// verbatim, so a pasted token never stops working (spec §59).
//
// A token value, refresh token, authorization code, or PKCE verifier must never
// appear in an error, a log, or a reply, so nothing in this file returns a
// secret to a caller that would display it (spec §22, §54).

const (
	// StoredTokenVersion identifies the JSON envelope. The relay_oauth2 key is
	// what marks a stored value as an envelope: a value whose version is not
	// this constant is not one this build understands, and DecodeStoredToken
	// treats it as a pasted token.
	StoredTokenVersion = 1

	// FlowBrowser and FlowDevice record which grant produced a token, so
	// relay auth status can tell the human how it was obtained and a refresh
	// can preserve the label.
	FlowBrowser = "browser"
	FlowDevice  = "device"

	// refreshGrantType is the grant type RFC 6749 §6 defines for exchanging a
	// refresh token.
	refreshGrantType = "refresh_token"
)

// StoredToken is the JSON envelope the daemon writes to the Keychain as the one
// stored string for an OAuth2 tool (spec §21, §22).
//
// Every field is optional except the version and the access token, so the
// envelope round-trips a token a server returned with no refresh token and no
// expiry just as faithfully as a full one. ExpiresAt is RFC3339 in UTC; empty
// means the authorization server told us no expiry, which is a different fact
// from "expired now" and must be preserved as such.
type StoredToken struct {
	Version       int    `json:"relay_oauth2"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	Scope         string `json:"scope,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	Flow          string `json:"flow,omitempty"`
}

// EncodeStoredToken renders the envelope as the single string that goes in the
// Keychain. It sets the version key so the value is self-identifying, and it
// refuses to produce an envelope with no access token rather than writing a
// value that could never authenticate a request.
func EncodeStoredToken(token StoredToken) (string, *relay.Error) {
	if strings.TrimSpace(token.AccessToken) == "" {
		return "", relay.NewError(relay.CodeInvalidInput,
			"the OAuth2 token envelope has no access_token")
	}
	token.Version = StoredTokenVersion
	encoded, err := json.Marshal(token)
	if err != nil {
		// The envelope holds only strings, so a marshal failure is a build bug,
		// not something the human can act on. The value is not echoed.
		return "", relay.NewError(relay.CodeRuntimeIncompatible,
			"the OAuth2 token envelope could not be encoded")
	}
	return string(encoded), nil
}

// DecodeStoredToken reads an envelope out of a stored value. ok is false, with
// no error, for any value that is not this envelope: not JSON, a different
// version, or missing an access token.
//
// This is the backward-compatibility hinge (spec §59). A token a human pasted
// is not JSON, so it falls through to ok=false and every reader uses it
// verbatim. There is deliberately no error path here: "not an envelope" is the
// normal case, not a failure.
func DecodeStoredToken(value string) (StoredToken, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed[0] != '{' {
		return StoredToken{}, false
	}
	var token StoredToken
	if err := json.Unmarshal([]byte(trimmed), &token); err != nil {
		return StoredToken{}, false
	}
	if token.Version != StoredTokenVersion || strings.TrimSpace(token.AccessToken) == "" {
		return StoredToken{}, false
	}
	return token, true
}

// NewStoredToken builds an envelope from a freshly obtained token. It converts
// the server's ExpiresIn into an absolute ExpiresAt so a later refresh can tell
// whether the token is still good without knowing when it was issued.
func NewStoredToken(token Token, flow, tokenEndpoint, clientID string) StoredToken {
	return StoredToken{
		Version:       StoredTokenVersion,
		AccessToken:   token.AccessToken,
		RefreshToken:  token.RefreshToken,
		TokenType:     token.TokenType,
		Scope:         token.Scope,
		ExpiresAt:     expiryFrom(token.ExpiresIn, time.Now()),
		TokenEndpoint: strings.TrimSpace(tokenEndpoint),
		ClientID:      strings.TrimSpace(clientID),
		Flow:          strings.TrimSpace(flow),
	}
}

// expiryFrom renders an absolute RFC3339 UTC expiry for a relative lifetime
// measured from now. A non-positive lifetime means the server declared no
// expiry, which this returns as the empty string rather than a zero date.
func expiryFrom(expiresIn time.Duration, now time.Time) string {
	if expiresIn <= 0 {
		return ""
	}
	return now.Add(expiresIn).UTC().Truncate(time.Second).Format(time.RFC3339)
}

// AccessValue is what goes on the wire as the credential. Callers that only
// present a credential use this and never see the refresh token (spec §22).
func (s StoredToken) AccessValue() string {
	return s.AccessToken
}

// Expiring reports whether the token is at or past its expiry once skew is
// applied — the daemon refreshes a little before the server would reject it.
//
// An empty or unreadable expiry is not a reason to refresh: unknown means the
// authorization server declared no expiry, and inventing one would either churn
// refreshes or skip a needed one. Unknown therefore reads as "not expiring".
func (s StoredToken) Expiring(now time.Time, skew time.Duration) bool {
	if strings.TrimSpace(s.ExpiresAt) == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, s.ExpiresAt)
	if err != nil {
		return false
	}
	return !now.Add(skew).Before(expiresAt)
}

// Describe renders a non-secret one-line summary for relay auth status. It
// must never contain a token value; it reports only the shape of what is stored
// (spec §22).
func (s StoredToken) Describe() string {
	label := "oauth2"
	if flow := strings.TrimSpace(s.Flow); flow != "" {
		label += " " + flow
	}
	label += " grant"

	parts := []string{label}
	if strings.TrimSpace(s.ExpiresAt) == "" {
		parts = append(parts, "expiry unknown")
	} else {
		parts = append(parts, "expires "+s.ExpiresAt)
	}
	if scope := strings.TrimSpace(s.Scope); scope != "" {
		parts = append(parts, "scope "+scope)
	}
	if strings.TrimSpace(s.RefreshToken) != "" {
		parts = append(parts, "refreshable")
	}
	return strings.Join(parts, ", ")
}

// Refreshing an access token (RFC 6749 §6) — spec §21, §54.
//
// Background refresh is why the envelope exists: a long-running daemon can swap
// an expired access token for a fresh one without a human. This is a public
// client, so there is deliberately no client secret — the same reasoning as
// DeviceFlowSpec. A refresh token is a secret, and no error here echoes it or
// the response body (spec §22, §54).

// RefreshOptions configures a RefreshClient. Now exists so tests can assert on
// a recomputed expiry instead of depending on wall-clock time.
type RefreshOptions struct {
	HTTP *http.Client
	Now  func() time.Time
}

// RefreshClient performs the one HTTP leg of a refresh. It is stateless apart
// from its timing policy.
type RefreshClient struct {
	http *http.Client
	now  func() time.Time
}

// NewRefreshClient returns a refresh client. Passing a nil HTTP client selects
// a bounded default so one unresponsive authorization server cannot pin the
// daemon forever.
func NewRefreshClient(options RefreshOptions) *RefreshClient {
	client := options.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &RefreshClient{http: client, now: now}
}

// RefreshSpec is everything a refresh needs beyond the stored envelope.
//
// Tool is daemon-supplied context, never parsed from a manifest; it exists only
// so an error can name the tool the human is trying to use.
type RefreshSpec struct {
	Tool          string
	TokenEndpoint string
	ClientID      string
	Scopes        []string
}

// validate reports the fields a refresh cannot run without. The message names
// fields, never values.
func (s RefreshSpec) validate() *relay.Error {
	var missing []string
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
		fmt.Sprintf("the OAuth2 refresh cannot run: the manifest is missing %s",
			strings.Join(missing, ", ")))
}

// Refresh exchanges the previous envelope's refresh token for a fresh access
// token (RFC 6749 §6).
//
// The envelope it returns is built so the next refresh can run: when the server
// does not rotate the refresh token, the previous one is carried forward rather
// than dropped, because losing it would silently break the next refresh. When
// the server omits expires_in, the new envelope says expiry unknown instead of
// inventing one.
func (c *RefreshClient) Refresh(ctx context.Context, spec RefreshSpec, previous StoredToken) (StoredToken, *relay.Error) {
	if failure := spec.validate(); failure != nil {
		return StoredToken{}, failure
	}
	if strings.TrimSpace(previous.RefreshToken) == "" {
		return StoredToken{}, relay.NewError(relay.CodeAuthRequired,
			fmt.Sprintf("%s has no refresh token stored; run 'relay auth login %s'",
				toolLabel(spec.Tool), toolLabel(spec.Tool)))
	}

	form := url.Values{}
	form.Set("grant_type", refreshGrantType)
	form.Set("refresh_token", previous.RefreshToken)
	form.Set("client_id", spec.ClientID)
	// RFC 6749 §6 lets the client narrow the requested scope; sending the
	// previously granted scope keeps the new token at least as broad as before.
	scope := strings.TrimSpace(previous.Scope)
	if scope == "" && len(spec.Scopes) > 0 {
		scope = strings.Join(spec.Scopes, " ")
	}
	if scope != "" {
		form.Set("scope", scope)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		spec.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return StoredToken{}, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("cannot build the refresh request: %v", err))
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return StoredToken{}, relay.NewError(relay.CodeTimeout, "the token refresh was cancelled")
		}
		return StoredToken{}, relay.NewError(relay.CodeNetworkError,
			scrub(fmt.Sprintf("the refresh request could not reach the authorization server: %v", err),
				previous.RefreshToken, previous.AccessToken))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, deviceResponseLimit))
	if err != nil {
		return StoredToken{}, relay.NewError(relay.CodeNetworkError, "could not read the refresh response")
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
		// The body is not echoed: a remote server could reflect a secret back.
		return StoredToken{}, relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the token endpoint returned a response this build cannot read (HTTP %d)",
				response.StatusCode))
	}
	if response.StatusCode >= 500 {
		return StoredToken{}, relay.NewError(relay.CodeRemoteError,
			fmt.Sprintf("the token endpoint failed with HTTP %d", response.StatusCode))
	}
	if payload.Error != "" {
		description := scrub(payload.ErrorDescription, previous.RefreshToken, previous.AccessToken)
		if payload.Error == "invalid_grant" {
			// invalid_grant means the refresh token is dead (RFC 6749 §5.2):
			// the human must log in again, which is a precondition, not a
			// service failure.
			message := fmt.Sprintf("the stored refresh token for %s is no longer valid; run 'relay auth login %s'",
				toolLabel(spec.Tool), toolLabel(spec.Tool))
			if description != "" {
				message += ": " + description
			}
			return StoredToken{}, relay.NewError(relay.CodeAuthRequired, message)
		}
		message := fmt.Sprintf("the authorization server rejected the token refresh for %s (%s)",
			toolLabel(spec.Tool), payload.Error)
		if description != "" {
			message += ": " + description
		}
		return StoredToken{}, relay.NewError(relay.CodeAuthFailed, message)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return StoredToken{}, relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("the token endpoint refused the refresh request (HTTP %d)", response.StatusCode))
	}
	if payload.AccessToken == "" {
		return StoredToken{}, relay.NewError(relay.CodeProtocolError,
			"the token endpoint returned no access token")
	}

	refreshed := StoredToken{
		Version:       StoredTokenVersion,
		AccessToken:   payload.AccessToken,
		RefreshToken:  previous.RefreshToken,
		TokenType:     previous.TokenType,
		Scope:         previous.Scope,
		ExpiresAt:     "",
		TokenEndpoint: strings.TrimSpace(spec.TokenEndpoint),
		ClientID:      strings.TrimSpace(spec.ClientID),
		Flow:          previous.Flow,
	}
	if payload.RefreshToken != "" {
		// The server rotated the refresh token; take the new one.
		refreshed.RefreshToken = payload.RefreshToken
	}
	if payload.TokenType != "" {
		refreshed.TokenType = payload.TokenType
	}
	if payload.Scope != "" {
		refreshed.Scope = payload.Scope
	}
	if expiresIn, ok := seconds(payload.ExpiresIn); ok {
		refreshed.ExpiresAt = expiryFrom(expiresIn, c.now())
	}
	return refreshed, nil
}
