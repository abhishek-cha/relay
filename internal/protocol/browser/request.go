package browser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"relay/internal/protocol"
	"relay/pkg/relay"
)

// This file mirrors the REST executor's request resolution -- {placeholder}
// substitution in the path, optional query and header templates, and a JSON
// body built from unconsumed inputs -- so a browser operation has exactly the
// shape a REST operation has and only the transport differs (spec §19, §23).
// The two are kept in step by the same spec section rather than by sharing
// unexported helpers across the package boundary.

// placeholderRe matches {name} placeholders in path, query, and header
// templates.
var placeholderRe = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// resolvePath resolves {placeholders} in the path and URL-escapes each
// substituted value. A missing required input is INVALID_INPUT.
func resolvePath(template string, input map[string]any) (string, error) {
	matches := placeholderRe.FindAllStringSubmatch(template, -1)
	if len(matches) == 0 {
		return template, nil
	}
	result := template
	for _, match := range matches {
		key := match[1]
		value, ok := input[key]
		if !ok {
			return "", relay.NewError(
				relay.CodeInvalidInput,
				fmt.Sprintf("missing required input %q for path placeholder", key))
		}
		result = strings.ReplaceAll(result, match[0], url.PathEscape(fmt.Sprintf("%v", value)))
	}
	return result, nil
}

// resolveOptional resolves {placeholders} in a template string. missing reports
// that a placeholder named an unsupplied input, so the caller skips the whole
// parameter rather than sending a half-resolved one.
func resolveOptional(template string, input map[string]any) (resolved string, missing bool) {
	matches := placeholderRe.FindAllStringSubmatch(template, -1)
	if len(matches) == 0 {
		return template, false
	}
	result := template
	for _, match := range matches {
		value, ok := input[match[1]]
		if !ok {
			return "", true
		}
		result = strings.ReplaceAll(result, match[0], fmt.Sprintf("%v", value))
	}
	return result, false
}

// methodHasBody reports whether a method carries a request body.
func methodHasBody(method string) bool {
	switch method {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

// buildBody builds the request body: the declared Spec.Body with placeholders
// resolved, or the unconsumed inputs as a JSON object when the manifest
// declares none.
func buildBody(req protocol.Request, consumed map[string]bool) ([]byte, error) {
	if req.Spec.Body != nil {
		return resolveBodyPlaceholders(req.Spec.Body, req.Input)
	}
	unconsumed := make(map[string]any)
	for key, value := range req.Input {
		if !consumed[key] {
			unconsumed[key] = value
		}
	}
	if len(unconsumed) == 0 {
		return nil, nil
	}
	return json.Marshal(unconsumed)
}

// resolveBodyPlaceholders walks a JSON-serialisable value and resolves
// {placeholder} tokens found in any string leaf.
func resolveBodyPlaceholders(body any, input map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, relay.NewError(relay.CodeInvalidInput, "failed to marshal Spec.Body")
	}
	resolved := placeholderRe.ReplaceAllStringFunc(string(raw), func(match string) string {
		value, ok := input[match[1:len(match)-1]]
		if !ok {
			return match
		}
		encoded, _ := json.Marshal(fmt.Sprintf("%v", value))
		if len(encoded) >= 2 && encoded[0] == '"' && encoded[len(encoded)-1] == '"' {
			return string(encoded[1 : len(encoded)-1])
		}
		return string(encoded)
	})

	var check any
	if err := json.Unmarshal([]byte(resolved), &check); err != nil {
		return nil, relay.NewError(
			relay.CodeInvalidInput,
			"resolved Spec.Body is not valid JSON after placeholder substitution")
	}
	return []byte(resolved), nil
}

// isJSONContentType reports whether a Content-Type denotes JSON.
func isJSONContentType(contentType string) bool {
	contentType = strings.TrimSpace(contentType)
	return strings.HasPrefix(contentType, "application/json") || strings.HasPrefix(contentType, "text/json")
}
