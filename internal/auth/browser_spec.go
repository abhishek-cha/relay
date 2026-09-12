package auth

import (
	"strings"

	"relay/pkg/relay"

	"gopkg.in/yaml.v3"
)

// Reading the browser flow's declaration out of a tool's manifest (spec §21,
// §23, §54).
//
// The browser grant is additive to the existing auth block: a manifest that
// already says auth: {type: oauth2, provider: github} keeps meaning "store a
// token a human pasted in", because it declares no endpoints. A manifest that
// additionally declares auth.authorizationEndpoint opts into the flow; naming
// that endpoint is what selects the browser grant over the device grant, and
// the manifest validator rejects declaring both. Every existing manifest stays
// valid and unchanged (spec §59).

// BrowserFlowSpecFromManifest extracts the browser flow specification from a
// tool's raw manifest.
//
// ok is false when the manifest does not declare an OAuth2 browser flow — no
// auth block, a different type, or an oauth2 block with no authorization
// endpoint. Those are the pre-existing manual-token (and device) paths and are
// not errors. A block that names an authorization endpoint but omits a required
// field is an error, so a half-written manifest fails loudly instead of
// silently falling back to pasting a token.
func BrowserFlowSpecFromManifest(manifestYAML []byte) (spec BrowserFlowSpec, ok bool, failure *relay.Error) {
	var document manifestAuthBlock
	if err := yaml.Unmarshal(manifestYAML, &document); err != nil {
		// The manifest body is not echoed: a malformed block could contain a
		// value a human meant to keep out of a diagnostic (spec §22).
		return BrowserFlowSpec{}, false, relay.NewError(relay.CodeRuntimeIncompatible,
			"the manifest could not be read for its auth block")
	}
	if document.Auth == nil || Canonical(document.Auth.Type) != "oauth2" {
		return BrowserFlowSpec{}, false, nil
	}

	authorizationEndpoint := strings.TrimSpace(document.Auth.AuthorizationEndpoint)
	if authorizationEndpoint == "" {
		// No authorization endpoint: a pasted token or a device flow. Either
		// way this is not a browser flow, and it is not an error.
		return BrowserFlowSpec{}, false, nil
	}

	spec = BrowserFlowSpec{
		AuthorizationEndpoint: authorizationEndpoint,
		TokenEndpoint:         strings.TrimSpace(document.Auth.TokenEndpoint),
		ClientID:              strings.TrimSpace(document.Auth.ClientID),
		Scopes:                normalizeScopes(document.Auth.Scopes),
		RedirectURI:           strings.TrimSpace(document.Auth.RedirectURI),
	}
	if failure := spec.validate(); failure != nil {
		return BrowserFlowSpec{}, false, failure
	}
	return spec, true, nil
}
