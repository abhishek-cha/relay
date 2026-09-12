package auth

import (
	"strings"

	"relay/pkg/relay"

	"gopkg.in/yaml.v3"
)

// Reading the device flow's declaration out of a tool's manifest (spec §21,
// §54).
//
// The device grant is additive to the existing auth block: a manifest that
// already says `auth: {type: oauth2, provider: github}` keeps meaning "store a
// token a human pasted in", because it declares no endpoints. A manifest that
// additionally declares the endpoints, client id, and scopes opts into the
// flow. That keeps every existing manifest valid and unchanged (spec §59).

// manifestAuthBlock is the subset of a manifest's auth block the device flow
// needs. It mirrors the additive fields from spec §21; the schema is owned by
// internal/manifest, and this reader stays in lockstep with it.
type manifestAuthBlock struct {
	Auth *struct {
		Type                        string `yaml:"type"`
		Provider                    string `yaml:"provider"`
		DeviceAuthorizationEndpoint string `yaml:"deviceAuthorizationEndpoint"`
		TokenEndpoint               string `yaml:"tokenEndpoint"`
		ClientID                    string `yaml:"clientId"`
		// AuthorizationEndpoint and RedirectURI select the browser
		// authorization-code + PKCE flow instead of the device grant. They are
		// read here only so the device reader can recognise a browser manifest
		// and refuse to misread it as a broken device one.
		AuthorizationEndpoint string `yaml:"authorizationEndpoint"`
		RedirectURI           string `yaml:"redirectURI"`
		Scopes                any    `yaml:"scopes"`
	} `yaml:"auth"`
}

// DeviceFlowSpecFromManifest extracts the device flow specification from a
// tool's raw manifest.
//
// ok is false when the manifest does not declare an OAuth2 device flow — no
// auth block, a different type, or an oauth2 block with no device endpoints.
// That is the pre-existing manual-token path and is not an error. A block that
// starts declaring a device flow but omits a required field is an error, so a
// half-written manifest fails loudly instead of silently falling back.
func DeviceFlowSpecFromManifest(manifestYAML []byte) (spec DeviceFlowSpec, ok bool, failure *relay.Error) {
	var document manifestAuthBlock
	if err := yaml.Unmarshal(manifestYAML, &document); err != nil {
		// The manifest body is not echoed: a malformed block could contain a
		// value a human meant to keep out of a diagnostic (spec §22).
		return DeviceFlowSpec{}, false, relay.NewError(relay.CodeRuntimeIncompatible,
			"the manifest could not be read for its auth block")
	}
	if document.Auth == nil || Canonical(document.Auth.Type) != "oauth2" {
		return DeviceFlowSpec{}, false, nil
	}
	if strings.TrimSpace(document.Auth.AuthorizationEndpoint) != "" {
		// A browser-flow manifest also declares tokenEndpoint and clientId,
		// which would otherwise look like a device declaration missing its
		// deviceAuthorizationEndpoint. The two flows are mutually exclusive,
		// so this is a browser manifest, not a broken device one.
		return DeviceFlowSpec{}, false, nil
	}

	spec = DeviceFlowSpec{
		DeviceAuthorizationEndpoint: strings.TrimSpace(document.Auth.DeviceAuthorizationEndpoint),
		TokenEndpoint:               strings.TrimSpace(document.Auth.TokenEndpoint),
		ClientID:                    strings.TrimSpace(document.Auth.ClientID),
		Scopes:                      normalizeScopes(document.Auth.Scopes),
	}
	if !spec.declared() {
		// Plain oauth2: a token a human pasted in, exactly as before.
		return DeviceFlowSpec{}, false, nil
	}
	if failure := spec.validate(); failure != nil {
		return DeviceFlowSpec{}, false, failure
	}
	return spec, true, nil
}

// declared reports whether the auth block asks for the device grant at all.
func (s DeviceFlowSpec) declared() bool {
	return s.DeviceAuthorizationEndpoint != "" || s.TokenEndpoint != "" ||
		s.ClientID != "" || len(s.Scopes) > 0
}

// normalizeScopes accepts both shapes a manifest author might write: a YAML
// list, or a single space-delimited string.
func normalizeScopes(raw any) []string {
	var scopes []string
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		for _, scope := range strings.Fields(value) {
			scopes = append(scopes, scope)
		}
	case []any:
		for _, item := range value {
			if scope, ok := item.(string); ok && strings.TrimSpace(scope) != "" {
				scopes = append(scopes, strings.TrimSpace(scope))
			}
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	return scopes
}
