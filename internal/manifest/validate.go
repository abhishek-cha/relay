package manifest

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Validation rules from spec §59: required fields, schema correctness,
// duplicate operation names, invalid protocol, invalid auth.

// knownProtocols is the relay/v1 vocabulary a manifest may declare. It is
// deliberately NOT the set this build can execute: whether an executor exists
// is a runtime question, answered by the daemon's executor map, which rejects
// an unimplemented protocol with PROTOCOL_ERROR (spec §19). Failing the build
// here instead would make that runtime path unreachable and would break a tool
// built for a newer runtime than the local daemon (spec §34, §35).
var (
	toolNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	pathParamPattern  = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
	knownProtocols    = map[string]bool{"rest": true, "graphql": true, "grpc": true, "browser": true, "local": true}
	knownAuthTypes    = map[string]bool{"api_key": true, "bearer": true, "basic": true, "oauth2": true, "client_credentials": true}
	knownCapabilities = map[string]bool{"network": true, "keychain": true, "browser": true, "filesystem.read": true, "filesystem.write": true, "shell": true, "notifications": true, "clipboard": true}
	allowedMethods    = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
)

// ValidationError aggregates every problem found, so one build reports the
// whole manifest rather than failing on the first issue.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("manifest invalid (%d problem(s)):\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Validate checks a manifest against the relay/v1 schema. It returns a
// *ValidationError listing all problems, or nil when the manifest is valid.
func (d *Document) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if d.APIVersion != APIVersion {
		add("apiVersion: want %q, got %q", APIVersion, d.APIVersion)
	}
	if d.Kind != KindTool {
		add("kind: want %q, got %q", KindTool, d.Kind)
	}
	if d.Metadata.Name == "" {
		add("metadata.name: required")
	} else if !toolNamePattern.MatchString(d.Metadata.Name) {
		add("metadata.name: %q must be lowercase snake_case", d.Metadata.Name)
	}
	if d.Metadata.Version == "" {
		add("metadata.version: required")
	}

	if d.Protocol.Type == "" {
		add("protocol.type: required")
	} else if !knownProtocols[d.Protocol.Type] {
		add("protocol.type: unknown protocol %q", d.Protocol.Type)
	}
	if d.Protocol.Type == "rest" && d.Protocol.BaseURL == "" {
		add("protocol.baseUrl: required for rest")
	}
	if d.Protocol.Type == "local" && d.Protocol.BaseURL != "" {
		add("protocol.baseUrl: not allowed for local; a local capability has no service")
	}
	if d.Protocol.Type == "graphql" && d.Protocol.Endpoint == "" {
		add("protocol.endpoint: required for graphql")
	}

	if d.Auth != nil {
		if d.Auth.Type == "" {
			add("auth.type: required when auth is present")
		} else if !knownAuthTypes[d.Auth.Type] {
			add("auth.type: unknown auth type %q", d.Auth.Type)
		}
		validateAuthBrowserFlow(d.Auth, add)
	}

	for _, capability := range d.Capabilities {
		if !knownCapabilities[capability] {
			add("capabilities: unknown capability %q", capability)
		}
	}

	if len(d.Tools) == 0 {
		add("tools: at least one operation is required")
	}

	seen := map[string]bool{}
	for i, tool := range d.Tools {
		where := fmt.Sprintf("tools[%d]", i)
		if tool.Name == "" {
			add("%s.name: required", where)
			continue
		}
		where = fmt.Sprintf("tools[%d] (%s)", i, tool.Name)

		if !toolNamePattern.MatchString(tool.Name) {
			add("%s.name: %q must be lowercase snake_case", where, tool.Name)
		}
		if seen[tool.Name] {
			add("%s.name: duplicate operation name %q", where, tool.Name)
		}
		seen[tool.Name] = true

		if tool.Description == "" {
			add("%s.description: required", where)
		}
		if tool.Input.Type != "" && tool.Input.Type != "object" {
			add("%s.input.type: want %q, got %q", where, "object", tool.Input.Type)
		}
		for _, required := range tool.Input.Required {
			if _, ok := tool.Input.Properties[required]; !ok {
				add("%s.input.required: %q is not a declared property", where, required)
			}
		}
		validateRequest(where, d.Protocol.Type, tool, add)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return &ValidationError{Problems: problems}
	}
	return nil
}

// validateAuthBrowserFlow checks the additive fields that declare an OAuth2
// browser authorization-code + PKCE flow (spec §21, §23, §24, §40, §54).
//
// Naming auth.authorizationEndpoint is what selects the flow, so that endpoint
// is meaningful only for auth.type oauth2 and only alongside the token endpoint
// and client id the code exchange needs. The browser flow is a public client: a
// browser flow that names a client secret is a mistake, and the manifest is not
// a secret store (spec §21, §22), so no secret field is read or required here.
// The browser and device grants are mutually exclusive, so declaring both
// endpoints at once is ambiguous and rejected rather than silently resolved.
//
// auth.redirectURI is optional — absent, Relay binds an ephemeral loopback port
// — but when present it must be a loopback http URL, because the daemon binds a
// 127.0.0.1 listener and nothing else can receive the authorization code safely
// (spec §23, §40). Every message names the field and never echoes the value.
func validateAuthBrowserFlow(auth *Auth, add func(string, ...any)) {
	if auth.AuthorizationEndpoint != "" {
		if auth.Type != "oauth2" {
			add("auth.authorizationEndpoint: only allowed for auth.type oauth2; it selects the browser authorization-code flow")
		}
		if auth.DeviceAuthorizationEndpoint != "" {
			add("auth.authorizationEndpoint: not allowed together with auth.deviceAuthorizationEndpoint; the browser and device flows are mutually exclusive")
		}
		if auth.TokenEndpoint == "" {
			add("auth.tokenEndpoint: required when auth.authorizationEndpoint is set")
		}
		if auth.ClientID == "" {
			add("auth.clientId: required when auth.authorizationEndpoint is set")
		}
	}

	if auth.RedirectURI != "" {
		if auth.AuthorizationEndpoint == "" {
			add("auth.redirectURI: requires auth.authorizationEndpoint; a redirect URI means nothing without a browser flow")
		}
		validateLoopbackRedirectURI(auth.RedirectURI, add)
	}
}

// validateLoopbackRedirectURI enforces the shape of a browser flow's callback
// URL (spec §23, §40). The daemon binds a loopback listener, so the redirect
// must be an absolute http URL whose host is 127.0.0.1 or localhost (an
// optional port is allowed) and which carries no query and no fragment. The
// value is never echoed: a redirect URI can embed a state or code parameter,
// and a diagnostic must not reflect it back (spec §22, §40).
func validateLoopbackRedirectURI(raw string, add func(string, ...any)) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || !strings.EqualFold(parsed.Scheme, "http") {
		add("auth.redirectURI: must be an absolute URL whose scheme is http; the daemon binds a loopback listener")
		return
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "127.0.0.1", "localhost":
	default:
		add("auth.redirectURI: host must be 127.0.0.1 or localhost; nothing else can receive the authorization code safely")
	}
	if strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		add("auth.redirectURI: must not carry a query or fragment")
	}
}

// validateRequest checks the shape of a single operation's request block. The
// rules are protocol-specific: REST resolves path/query/header templates,
// gRPC names its method by path or by literal package/service/method, GraphQL
// posts a document plus variable bindings, and a local capability names the
// primitive it runs (spec §19, §20, §44, §45, §46). Browser, which still has no
// defined request shape, is checked against the REST rules, which keeps those
// manifests loadable until its executor defines a shape.
func validateRequest(where, protocolType string, tool Tool, add func(string, ...any)) {
	// request.package and request.service address a gRPC method by name
	// (spec §45). They are meaningful only for grpc, so a manifest that declares
	// them on any other protocol is rejected rather than silently ignored.
	// request.method cannot be checked here: it is every REST operation's HTTP
	// verb, so only the two new fields are unambiguous on a non-gRPC protocol.
	if protocolType != "grpc" {
		if tool.Request.Package != "" {
			add("%s.request.package: not allowed for %s; it addresses a gRPC method", where, protocolType)
		}
		if tool.Request.Service != "" {
			add("%s.request.service: not allowed for %s; it addresses a gRPC method", where, protocolType)
		}
	}

	switch protocolType {
	case "grpc":
		validateGRPCRequest(where, tool, add)
	case "graphql":
		validateGraphQLRequest(where, tool, add)
		rejectPagination("graphql", where, tool, add)
	case "local":
		validateLocalRequest(where, tool, add)
		rejectPagination("local", where, tool, add)
	default:
		validateRESTRequest(where, tool, add)
		validatePagination(where, tool, add)
	}
}

// validateGRPCRequest checks how an operation addresses its gRPC method
// (spec §45). A gRPC call has no HTTP verb, so an operation may either keep the
// path-addressed form — request.method as the vestigial HTTP verb plus
// request.path as /package.Service/Method, exactly as before — or declare the
// literal triple request.package / request.service / request.method, naming the
// method the way the protocol block names a service. The two forms are mutually
// exclusive: declaring both is ambiguous and is rejected rather than silently
// picking one, so a manifest always says exactly where its method lives.
//
// request.method carries the RPC method name in the literal form because there
// is no HTTP verb for it to carry. A method set alongside a path is the legacy
// HTTP verb, not a literal gRPC method name, which keeps every path-addressed
// operation valid unchanged.
func validateGRPCRequest(where string, tool Tool, add func(string, ...any)) {
	request := tool.Request
	literal := request.Package != "" || request.Service != "" ||
		(request.Method != "" && request.Path == "")
	if !literal {
		// Path-addressed: today's shape, unchanged.
		validateRESTRequest(where, tool, add)
		validatePagination(where, tool, add)
		return
	}

	if request.Path != "" {
		add("%s.request.path: not allowed together with request.package/request.service/request.method; declare the literal method or the path, not both", where)
	}
	if request.Package == "" {
		add("%s.request.package: required when addressing a gRPC method by name", where)
	}
	if request.Service == "" {
		add("%s.request.service: required when addressing a gRPC method by name", where)
	}
	if request.Method == "" {
		add("%s.request.method: required when addressing a gRPC method by name", where)
	}

	// The three must assemble into the canonical /package.Service/Method, so
	// none may itself be empty or carry a separator the assembled path would
	// misread.
	segments := []struct {
		field string
		value string
	}{
		{"package", request.Package},
		{"service", request.Service},
		{"method", request.Method},
	}
	for _, segment := range segments {
		if segment.value != "" && strings.ContainsAny(segment.value, "/ \t") {
			add("%s.request.%s: %q must not contain '/', a space, or a tab", where, segment.field, segment.value)
		}
	}

	validatePagination(where, tool, add)
}

// rejectPagination refuses a pagination block on a protocol that cannot act on
// it (spec §20). Pagination describes walking a REST collection by following a
// Link header or echoing a cursor; a GraphQL document returns one response and a
// local capability has no service to page, so the block would be a declaration
// nothing honours. It is rejected rather than ignored so a manifest cannot
// appear to page when it does not.
func rejectPagination(protocolType, where string, tool Tool, add func(string, ...any)) {
	if tool.Request.Pagination != nil {
		add("%s.request.pagination: not allowed for %s; pagination is a REST strategy", where, protocolType)
	}
}

// validatePagination checks a REST pagination block (spec §20). Every field is
// scoped to a style: a cursor strategy needs a parameter and a response field to
// carry the cursor, while a link-header strategy derives its cursor from the
// response header and so must not declare fields it will never read.
func validatePagination(where string, tool Tool, add func(string, ...any)) {
	p := tool.Request.Pagination
	if p == nil {
		return
	}

	if tool.Request.Method != "" {
		method := strings.ToUpper(tool.Request.Method)
		if method != "GET" && method != "HEAD" {
			add("%s.request.pagination: not allowed for method %s; pagination walks a GET or HEAD collection", where, method)
		}
	}

	switch p.Style {
	case "":
		add("%s.request.pagination.style: required (%s or %s)", where, PaginationStyleLinkHeader, PaginationStyleCursor)
	case PaginationStyleLinkHeader:
		if p.CursorParam != "" {
			add("%s.request.pagination.cursorParam: not allowed for link-header", where)
		}
		if p.CursorIn != "" {
			add("%s.request.pagination.cursorIn: not allowed for link-header", where)
		}
		if p.CursorField != "" {
			add("%s.request.pagination.cursorField: not allowed for link-header", where)
		}
		if p.HasMoreField != "" {
			add("%s.request.pagination.hasMoreField: not allowed for link-header", where)
		}
	case PaginationStyleCursor:
		if p.CursorParam == "" {
			add("%s.request.pagination.cursorParam: required for cursor", where)
		}
		if p.CursorField == "" {
			add("%s.request.pagination.cursorField: required for cursor", where)
		}
		if p.CursorIn != "" && p.CursorIn != CursorInQuery && p.CursorIn != CursorInBody {
			add("%s.request.pagination.cursorIn: %q must be %s or %s", where, p.CursorIn, CursorInQuery, CursorInBody)
		}
	default:
		add("%s.request.pagination.style: unknown style %q", where, p.Style)
	}

	if p.Limit < 0 {
		add("%s.request.pagination.limit: %d must not be negative", where, p.Limit)
	}
	if p.Limit > 0 && p.LimitParam == "" {
		add("%s.request.pagination.limit: requires limitParam", where)
	}
}

// validateLocalRequest checks the fields a local capability needs, and the
// address-shaped fields it must not carry (spec §19, §46).
//
// A local operation does not contact a service, so its request block names the
// primitive to invoke in request.operation and sets nothing that would describe
// a transport. A method, path, document, or variable binding here is a manifest
// author reaching for a remote shape by mistake, so it is rejected rather than
// ignored; the executor would otherwise silently run something other than what
// the manifest appears to describe.
//
// The validator checks that request.operation is present, not that this build
// implements it: whether an executor exists is a runtime question, answered by
// the executor registry with PROTOCOL_ERROR (spec §19), exactly as an unknown
// protocol.type is. That keeps a tool built for a newer runtime installable.
func validateLocalRequest(where string, tool Tool, add func(string, ...any)) {
	if tool.Request.Operation == "" {
		add("%s.request.operation: required for local", where)
	}
	if tool.Request.Method != "" {
		add("%s.request.method: not allowed for local; name the primitive with request.operation", where)
	}
	if tool.Request.Path != "" {
		add("%s.request.path: not allowed for local; name the primitive with request.operation", where)
	}
	if tool.Request.Document != "" {
		add("%s.request.document: not allowed for local", where)
	}
	if len(tool.Request.Variables) > 0 {
		add("%s.request.variables: not allowed for local", where)
	}
}

func validateRESTRequest(where string, tool Tool, add func(string, ...any)) {
	if tool.Request.Method == "" {
		add("%s.request.method: required", where)
	} else if !allowedMethods[strings.ToUpper(tool.Request.Method)] {
		add("%s.request.method: unsupported method %q", where, tool.Request.Method)
	}
	if tool.Request.Path == "" {
		add("%s.request.path: required", where)
		return
	}
	if !strings.HasPrefix(tool.Request.Path, "/") {
		add("%s.request.path: %q must start with /", where, tool.Request.Path)
	}

	required := map[string]bool{}
	for _, name := range tool.Input.Required {
		required[name] = true
	}
	for _, match := range pathParamPattern.FindAllStringSubmatch(tool.Request.Path, -1) {
		param := match[1]
		if _, ok := tool.Input.Properties[param]; !ok {
			add("%s.request.path: {%s} has no matching input property", where, param)
			continue
		}
		if !required[param] {
			add("%s.request.path: {%s} must be listed in input.required", where, param)
		}
	}
}

// graphQLVariablePattern matches a GraphQL variable reference or declaration,
// e.g. $id in `query GetUser($id: ID!) { user(id: $id) { name } }`. It is a
// lexical approximation: variables are the only `$name` tokens a document uses,
// so it is enough to know which inputs a query needs (spec §44).
var graphQLVariablePattern = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)

// validateGraphQLRequest checks the fields the GraphQL executor actually needs:
// a document, and an input property for every variable the document uses.
func validateGraphQLRequest(where string, tool Tool, add func(string, ...any)) {
	if tool.Request.Document == "" {
		add("%s.request.document: required for graphql", where)
	}

	used := graphQLVariables(tool.Request.Document)
	inDocument := make(map[string]bool, len(used))
	for _, name := range used {
		inDocument[name] = true
		property := resolveVariableProperty(name, tool.Request.Variables)
		if _, ok := tool.Input.Properties[property]; !ok {
			add("%s.request.document: $%s resolves to input property %q, which is not declared", where, name, property)
		}
	}

	// A mapping entry that the document never uses is still checked, so a typo
	// in the mapping is caught rather than silently ignored. Entries for used
	// variables were already covered above.
	for name := range tool.Request.Variables {
		if inDocument[name] {
			continue
		}
		property := resolveVariableProperty(name, tool.Request.Variables)
		if _, ok := tool.Input.Properties[property]; !ok {
			add("%s.request.variables: %q maps to input property %q, which is not declared", where, name, property)
		}
	}
}

// resolveVariableProperty returns the input property a GraphQL variable reads
// from. The mapping wins when it names one; otherwise the variable name is the
// property name, matching the executor's resolution exactly.
func resolveVariableProperty(variable string, mapping map[string]string) string {
	if property, ok := mapping[variable]; ok && property != "" {
		return property
	}
	return variable
}

// graphQLVariables returns the sorted, de-duplicated variable names a document
// references.
func graphQLVariables(document string) []string {
	seen := map[string]bool{}
	var names []string
	for _, match := range graphQLVariablePattern.FindAllStringSubmatch(document, -1) {
		name := match[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
