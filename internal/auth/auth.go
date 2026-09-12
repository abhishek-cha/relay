package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"relay/internal/keychain"
	"relay/internal/manifest"
	"relay/internal/protocol"
	"relay/pkg/relay"
)

// Presentation is how a resolved credential is attached to an outbound
// request: which header carries it and what scheme prefixes the value.
//
// It is deliberately protocol-neutral. The daemon turns a Presentation into a
// protocol.Credential and every Executor attaches it the same way, so no
// executor has to branch on the declared auth type (spec §19, §21).
type Presentation struct {
	Header string
	Scheme string
}

// presentations maps a manifest auth type (spec §21) to its conventional
// header presentation.
//
// The manifest's Auth struct has only Type and Provider, so there is no
// per-tool place to declare a custom header yet; these are the conventional
// defaults. api_key uses X-API-Key rather than Authorization because a bare
// key in Authorization is ambiguous with the scheme-prefixed forms. basic and
// client_credentials both use HTTP Basic, which is exactly how RFC 6749 §2.3.1
// presents client credentials. oauth2 stores an already-issued access token,
// which is a bearer token on the wire.
var presentations = map[string]Presentation{
	"api_key":            {Header: "X-API-Key"},
	"bearer":             {Header: "Authorization", Scheme: "Bearer"},
	"basic":              {Header: "Authorization", Scheme: "Basic"},
	"client_credentials": {Header: "Authorization", Scheme: "Basic"},
	"oauth2":             {Header: "Authorization", Scheme: "Bearer"},
}

// authAliases folds equivalent spellings onto one canonical type. The
// manifest validator accepts "bearer"; "bearer_token" is tolerated here so a
// manifest written against the looser spelling resolves identically.
var authAliases = map[string]string{
	"bearer_token": "bearer",
}

// Resolver turns a tool's declared auth into a credential the daemon injects
// at execution time, resolving the secret from the Keychain (spec §21, §22).
//
// A Resolver never returns a secret to a caller other than the executor that
// needs it, and no error it produces embeds one (spec §22, §54).
type Resolver struct {
	store keychain.Store
}

// New returns a Resolver backed by store. The daemon supplies the macOS
// Keychain in production and a fake in tests (spec §40).
func New(store keychain.Store) *Resolver {
	return &Resolver{store: store}
}

// Declared reports a tool's auth requirement. ok is false when the manifest
// declares no auth block, or declares one with an empty type — in that case the
// tool runs unauthenticated.
func Declared(doc *manifest.Document) (manifest.Auth, bool) {
	if doc == nil || doc.Auth == nil || strings.TrimSpace(doc.Auth.Type) == "" {
		return manifest.Auth{}, false
	}
	return *doc.Auth, true
}

// Canonical normalizes an auth-type spelling, folding aliases like
// "bearer_token" onto their canonical form.
func Canonical(authType string) string {
	normalized := strings.ToLower(strings.TrimSpace(authType))
	if canonical, ok := authAliases[normalized]; ok {
		return canonical
	}
	return normalized
}

// Supported reports whether this build knows how to present an auth type.
func Supported(authType string) bool {
	_, ok := presentations[Canonical(authType)]
	return ok
}

// Resolve maps a tool's declared auth onto a concrete credential and reads the
// secret from the Keychain.
//
// The return value distinguishes three outcomes that the call site must treat
// differently (spec §26):
//
//   - (nil, nil): the tool declares no auth. It runs unauthenticated, exactly
//     as it did before M4.
//   - (nil, AUTH_REQUIRED): the tool declares auth but no credential is stored.
//     No request is sent — a missing credential is a user-actionable
//     precondition ("run relay auth login"), not a service failure. Detecting
//     it locally avoids a wasted round-trip and a misleading 401 from the
//     service.
//   - (nil, AUTH_FAILED): the Keychain read itself failed. The credential may
//     exist but is unusable, which is a different remedy than "log in".
//
// A credential the service rejects at request time is neither of these; the
// daemon maps that to AUTH_FAILED after execution because only the daemon knows
// a credential was injected (see Daemon.invoke).
func (r *Resolver) Resolve(doc *manifest.Document, tool string) (*protocol.Credential, *relay.Error) {
	declared, ok := Declared(doc)
	if !ok {
		return nil, nil
	}

	canonical := Canonical(declared.Type)
	presentation, known := presentations[canonical]
	if !known {
		return nil, relay.NewError(relay.CodeAuthFailed,
			fmt.Sprintf("tool %q declares auth type %q, which this Relay build cannot present",
				tool, declared.Type))
	}

	secret, err := r.store.Get(keychain.Service(tool), keychain.AccountDefault)
	if err != nil {
		// A well-behaved Store returns no value with an error, but scrub the
		// value anyway in case a faulty implementation embeds it (spec §22).
		if errors.Is(err, keychain.ErrNotFound) {
			return nil, authRequired(tool, declared)
		}
		return nil, relay.NewError(relay.CodeAuthFailed,
			keychain.Redact(fmt.Sprintf("could not read the stored credential for %q: %v", tool, err), secret))
	}
	if secret == "" {
		return nil, authRequired(tool, declared)
	}

	return &protocol.Credential{
		Type:   canonical,
		Secret: encodeSecret(canonical, secret),
		Header: presentation.Header,
		Scheme: presentation.Scheme,
	}, nil
}

// Present reports whether a credential is stored for a tool. It deliberately
// returns only a boolean: the secret is never read out to build it (spec §22).
func (r *Resolver) Present(tool string) (bool, *relay.Error) {
	secret, err := r.store.Get(keychain.Service(tool), keychain.AccountDefault)
	if err == nil {
		return secret != "", nil
	}
	if errors.Is(err, keychain.ErrNotFound) {
		return false, nil
	}
	return false, relay.NewError(relay.CodeAuthFailed,
		keychain.Redact(fmt.Sprintf("could not read the stored credential for %q: %v", tool, err), secret))
}

// Stored reads a tool's token envelope. ok is false, with no error, when
// nothing is stored or the stored value is not an envelope — a token a human
// pasted is not one, and callers must fall back to the manual path (spec §59).
//
// The Keychain read error path mirrors Resolve and Present: a missing item is
// the ordinary "not logged in" case, and any other failure is AUTH_FAILED with
// the value scrubbed (spec §22).
func (r *Resolver) Stored(tool string) (StoredToken, bool, *relay.Error) {
	secret, err := r.store.Get(keychain.Service(tool), keychain.AccountDefault)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return StoredToken{}, false, nil
		}
		return StoredToken{}, false, relay.NewError(relay.CodeAuthFailed,
			keychain.Redact(fmt.Sprintf("could not read the stored credential for %q: %v", tool, err), secret))
	}
	stored, ok := DecodeStoredToken(secret)
	if !ok {
		return StoredToken{}, false, nil
	}
	return stored, true, nil
}

// Set stores a tool's credential. The secret is written to the Keychain and is
// never echoed in the returned error (spec §22, §54).
func (r *Resolver) Set(tool, secret string) *relay.Error {
	if secret == "" {
		return relay.NewError(relay.CodeInvalidInput, "the credential is empty")
	}
	if err := r.store.Set(keychain.Service(tool), keychain.AccountDefault, secret); err != nil {
		return relay.NewError(relay.CodeAuthFailed,
			keychain.Redact(fmt.Sprintf("could not store the credential for %q: %v", tool, err), secret))
	}
	return nil
}

// Clear removes a tool's credential. Deleting a credential that is not stored
// is not an error — the desired end state is reached either way.
func (r *Resolver) Clear(tool string) *relay.Error {
	if err := r.store.Delete(keychain.Service(tool), keychain.AccountDefault); err != nil {
		return relay.NewError(relay.CodeAuthFailed,
			keychain.Redact(fmt.Sprintf("could not clear the credential for %q: %v", tool, err)))
	}
	return nil
}

// authRequired is the AUTH_REQUIRED error for a declared-but-unstored
// credential. Its message and details carry only the auth type, never a secret.
func authRequired(tool string, declared manifest.Auth) *relay.Error {
	return relay.NewError(relay.CodeAuthRequired,
		fmt.Sprintf("%s requires %s authentication; run 'relay auth login %s'",
			tool, declared.Type, tool)).
		WithDetails(map[string]any{"authType": declared.Type})
}

// encodeSecret applies the per-type transformation that turns the stored value
// into the wire value.
//
// HTTP Basic carries base64(user:pass), and the stored value is the raw
// "user:pass" pair (or client_id:client_secret).
//
// For the token-bearing types (oauth2, bearer) the stored value may be a token
// envelope. When it is, the wire value is the envelope's access token, which is
// how a background refresh can recover the refresh token that a bare string
// would have thrown away (spec §21, §22). When it is not, the stored value is a
// token a human pasted, and the fallback returns it verbatim so pasted tokens
// keep working unchanged (spec §59).
func encodeSecret(canonical, secret string) string {
	switch canonical {
	case "basic", "client_credentials":
		return base64.StdEncoding.EncodeToString([]byte(secret))
	case "oauth2", "bearer":
		if stored, ok := DecodeStoredToken(secret); ok {
			return stored.AccessValue()
		}
		return secret
	default:
		return secret
	}
}
