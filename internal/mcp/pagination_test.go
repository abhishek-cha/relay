package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"relay/internal/manifest"
	"relay/pkg/relay"
)

// paginationDoc is a manifest with one paginated and one plain operation, so a
// test can prove the reserved argument is advertised on exactly the operations
// that declare a pagination strategy (spec §20, §27).
func paginationDoc() *manifest.Document {
	return &manifest.Document{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTool,
		Metadata:   manifest.Metadata{Name: "pagedemo", Version: "1.0.0"},
		Runtime:    manifest.Runtime{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
		Protocol:   manifest.Protocol{Type: "rest"},
		Tools: []manifest.Tool{
			{
				Name:        "list_pages",
				Description: "List a paged collection",
				Input: manifest.InputSchema{
					Type:       "object",
					Properties: map[string]manifest.Property{"page": {Type: "string"}},
				},
				Request: manifest.Request{
					Method:     "GET",
					Path:       "/pages",
					Pagination: &manifest.Pagination{Style: manifest.PaginationStyleLinkHeader},
				},
			},
			{
				Name:        "get_one",
				Description: "Get one thing",
				Input: manifest.InputSchema{
					Type:       "object",
					Properties: map[string]manifest.Property{"id": {Type: "string"}},
					Required:   []string{"id"},
				},
				Request: manifest.Request{Method: "GET", Path: "/one"},
			},
		},
	}
}

func toolByName(t *testing.T, tools []any, name string) map[string]any {
	t.Helper()
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tool is %T, want object", raw)
		}
		if tool["name"] == name {
			return tool
		}
	}
	t.Fatalf("tool %q not advertised in %#v", name, tools)
	return nil
}

func toolSchema(t *testing.T, tool map[string]any) map[string]any {
	t.Helper()
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("tool %v has no inputSchema object: %#v", tool["name"], tool)
	}
	return schema
}

func schemaProperties(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties object: %#v", schema)
	}
	return props
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestToolsListAdvertisesPaginateOnlyWhenDeclared pins requirement 1: the
// reserved argument appears on exactly the operations whose manifest declares a
// pagination strategy, stays optional, and leaves the rest of the schema (the
// manifest's own properties and required list) untouched.
func TestToolsListAdvertisesPaginateOnlyWhenDeclared(t *testing.T) {
	descriptors := descriptorsFor("pagedemo", paginationDoc())
	if !descriptors[0].Paginated {
		t.Fatal("list_pages must project Paginated=true")
	}
	if descriptors[1].Paginated {
		t.Fatal("get_one must project Paginated=false")
	}

	s := newServer(&stubSource{tools: descriptors}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"2.0","id":20,"method":"tools/list"}`)
	tools, ok := resultMap(t, requireOne(t, responses))["tools"].([]any)
	if !ok {
		t.Fatalf("tools is not an array")
	}

	pagedSchema := toolSchema(t, toolByName(t, tools, "pagedemo_list_pages"))
	props := schemaProperties(t, pagedSchema)
	if _, ok := props["page"]; !ok {
		t.Fatalf("manifest property dropped from paginated schema: %#v", props)
	}
	advertised, ok := props[PaginateArg].(map[string]any)
	if !ok || advertised["type"] != "boolean" {
		t.Fatalf("paginating operation must advertise %q as a boolean: %#v", PaginateArg, props)
	}
	if raw, _ := pagedSchema["required"].([]any); containsString(stringsOf(raw), PaginateArg) {
		t.Fatalf("%q must never be required: %#v", PaginateArg, pagedSchema["required"])
	}

	plainSchema := toolSchema(t, toolByName(t, tools, "pagedemo_get_one"))
	if _, ok := schemaProperties(t, plainSchema)[PaginateArg]; ok {
		t.Fatalf("non-paginated operation must not advertise %q: %#v", PaginateArg, plainSchema)
	}

	// tools/list must not mutate the cached descriptor it advertises from.
	if declared, _ := descriptors[0].InputSchema["properties"].(map[string]any); declared != nil {
		if _, mutated := declared[PaginateArg]; mutated {
			t.Fatal("tools/list mutated the cached descriptor schema")
		}
	}
}

func stringsOf(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, fmt.Sprint(value))
	}
	return out
}

// TestToolsCallThreadsPaginateToDaemon pins requirement 2: the reserved
// argument reaches the daemon as InvokeRequest.Paginate and is stripped before
// the operation input is handed over.
func TestToolsCallThreadsPaginateToDaemon(t *testing.T) {
	ops := &fakeOps{resp: relay.InvokeResponse{Success: true, Result: map[string]any{"items": []any{"a", "b"}}, Pages: 1}}
	s := newServer(&stubSource{tools: descriptorsFor("pagedemo", paginationDoc())}, DaemonInvoker{Operations: ops})

	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"pagedemo_list_pages","arguments":{"paginate":true,"page":"2"}}}`)
	if resp := requireOne(t, responses); resp.Error != nil {
		t.Fatalf("protocol fault: %#v", resp.Error)
	}
	if !ops.req.Paginate {
		t.Fatal("daemon did not receive InvokeRequest.Paginate=true")
	}
	if _, leaked := ops.req.Input[PaginateArg]; leaked {
		t.Fatalf("reserved argument leaked into operation input: %#v", ops.req.Input)
	}
	if ops.req.Input["page"] != "2" {
		t.Fatalf("operation input = %#v, want page=2", ops.req.Input)
	}
	if ops.req.Tool != "pagedemo" || ops.req.Operation != "list_pages" {
		t.Fatalf("request = %#v", ops.req)
	}
}

// TestToolsCallPaginateDefaultsOff proves paginate false or absent keeps the
// single-page default: no walk opt-in reaches the daemon and the result carries
// no walk shape.
func TestToolsCallPaginateDefaultsOff(t *testing.T) {
	for _, args := range []string{`{}`, `{"paginate":false}`} {
		ops := &fakeOps{resp: relay.InvokeResponse{Success: true, Result: map[string]any{"items": []any{"a"}}, Pages: 1}}
		s := newServer(&stubSource{tools: descriptorsFor("pagedemo", paginationDoc())}, DaemonInvoker{Operations: ops})
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"pagedemo_list_pages","arguments":%s}}`, args)
		_, responses := serve(t, s, frame)
		resp := requireOne(t, responses)
		if resp.Error != nil {
			t.Fatalf("args %s: protocol fault %#v", args, resp.Error)
		}
		if ops.req.Paginate {
			t.Fatalf("args %s: Paginate=true, want the single-page default", args)
		}
		if _, ok := resultMap(t, resp)["pages"]; ok {
			t.Fatalf("args %s: single-page result must not carry a walk shape", args)
		}
	}
}

// TestToolsCallReportsWalkShape proves requirement 3: Pages and Truncated reach
// the MCP caller inside the tool result while the machine result stays the JSON
// content (spec §20).
func TestToolsCallReportsWalkShape(t *testing.T) {
	ops := &fakeOps{resp: relay.InvokeResponse{
		Success:   true,
		Result:    map[string]any{"items": []any{"a", "b", "c", "d", "e"}},
		Pages:     3,
		Truncated: true,
	}}
	s := newServer(&stubSource{tools: descriptorsFor("pagedemo", paginationDoc())}, DaemonInvoker{Operations: ops})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"pagedemo_list_pages","arguments":{"paginate":true}}}`)

	resp := requireOne(t, responses)
	m := resultMap(t, resp)
	if m["pages"] != float64(3) {
		t.Fatalf("pages = %#v, want 3", m["pages"])
	}
	if m["truncated"] != true {
		t.Fatalf("truncated = %#v, want true", m["truncated"])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(contentText(t, resp)), &got); err != nil {
		t.Fatalf("content is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, roundTrip(t, ops.resp.Result)) {
		t.Fatalf("content = %#v, want the machine result", got)
	}
}

// TestPaginateReservedNamePrecedence pins the reserved-name contract: on a
// paginating operation the reserved meaning wins and an input property of the
// same name is unreachable; on a non-paginating operation the name is not
// reserved, so a declared "paginate" input passes through verbatim.
func TestPaginateReservedNamePrecedence(t *testing.T) {
	paginating := &manifest.Document{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTool,
		Metadata:   manifest.Metadata{Name: "tricky", Version: "1.0.0"},
		Runtime:    manifest.Runtime{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
		Protocol:   manifest.Protocol{Type: "rest"},
		Tools: []manifest.Tool{{
			Name: "walk",
			Input: manifest.InputSchema{
				Type:       "object",
				Properties: map[string]manifest.Property{PaginateArg: {Type: "string"}},
			},
			Request: manifest.Request{
				Method:     "GET",
				Path:       "/walk",
				Pagination: &manifest.Pagination{Style: manifest.PaginationStyleLinkHeader},
			},
		}},
	}
	ops := &fakeOps{resp: relay.InvokeResponse{Success: true, Result: "ok", Pages: 2}}
	s := newServer(&stubSource{tools: descriptorsFor("tricky", paginating)}, DaemonInvoker{Operations: ops})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"tricky_walk","arguments":{"paginate":true}}}`)
	if resp := requireOne(t, responses); resp.Error != nil {
		t.Fatalf("protocol fault: %#v", resp.Error)
	}
	if !ops.req.Paginate {
		t.Fatal("reserved meaning must win on a paginating operation")
	}
	if _, leaked := ops.req.Input[PaginateArg]; leaked {
		t.Fatalf("reserved argument must be stripped from input: %#v", ops.req.Input)
	}

	plain := &manifest.Document{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTool,
		Metadata:   manifest.Metadata{Name: "tricky", Version: "1.0.0"},
		Runtime:    manifest.Runtime{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
		Protocol:   manifest.Protocol{Type: "rest"},
		Tools: []manifest.Tool{{
			Name: "store",
			Input: manifest.InputSchema{
				Type:       "object",
				Properties: map[string]manifest.Property{PaginateArg: {Type: "string"}},
			},
			Request: manifest.Request{Method: "GET", Path: "/store"},
		}},
	}
	ops2 := &fakeOps{resp: relay.InvokeResponse{Success: true, Result: "ok"}}
	s2 := newServer(&stubSource{tools: descriptorsFor("tricky", plain)}, DaemonInvoker{Operations: ops2})
	_, responses2 := serve(t, s2,
		`{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"tricky_store","arguments":{"paginate":"keep-me"}}}`)
	if resp := requireOne(t, responses2); resp.Error != nil {
		t.Fatalf("protocol fault: %#v", resp.Error)
	}
	if ops2.req.Paginate {
		t.Fatal("a non-paginating operation must not read paginate as a walk opt-in")
	}
	if ops2.req.Input[PaginateArg] != "keep-me" {
		t.Fatalf("declared paginate input = %#v, want it delivered verbatim", ops2.req.Input)
	}
}

// TestToolsCallPaginateMustBeBoolean: a non-boolean reserved argument is
// rejected as INVALID_INPUT before the daemon is asked to do anything.
func TestToolsCallPaginateMustBeBoolean(t *testing.T) {
	ops := &fakeOps{}
	s := newServer(&stubSource{tools: descriptorsFor("pagedemo", paginationDoc())}, DaemonInvoker{Operations: ops})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"pagedemo_list_pages","arguments":{"paginate":"yes"}}}`)
	m := resultMap(t, requireOne(t, responses))
	if m["isError"] != true {
		t.Fatalf("isError = %#v, want true", m["isError"])
	}
	var structured relay.Error
	if err := json.Unmarshal([]byte(contentText(t, requireOne(t, responses))), &structured); err != nil {
		t.Fatalf("tool error is not a relay.Error: %v", err)
	}
	if structured.Code != relay.CodeInvalidInput {
		t.Fatalf("code = %q, want INVALID_INPUT", structured.Code)
	}
	if ops.req.Tool != "" {
		t.Fatalf("daemon must not be called for a malformed reserved argument: %#v", ops.req)
	}
}

// TestToolsCallPaginateWithoutSupportIsRefused: an invoker that does not carry
// the opt-in must refuse a requested walk rather than return a single page that
// would look complete (spec §20).
func TestToolsCallPaginateWithoutSupportIsRefused(t *testing.T) {
	inv := &stubInvoker{result: "ok"} // does not implement PaginatingInvoker
	s := newServer(&stubSource{tools: descriptorsFor("pagedemo", paginationDoc())}, inv)
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"pagedemo_list_pages","arguments":{"paginate":true}}}`)
	resp := requireOne(t, responses)
	if resp.Error != nil {
		t.Fatalf("must be a tool error, not a protocol fault: %#v", resp.Error)
	}
	m := resultMap(t, resp)
	if m["isError"] != true {
		t.Fatalf("isError = %#v, want true", m["isError"])
	}
	var structured relay.Error
	if err := json.Unmarshal([]byte(contentText(t, resp)), &structured); err != nil {
		t.Fatalf("tool error is not a relay.Error: %v", err)
	}
	if structured.Code != relay.CodeInvalidInput {
		t.Fatalf("code = %q, want INVALID_INPUT", structured.Code)
	}
	if inv.called {
		t.Fatal("invoker must not run a call it cannot paginate")
	}
}
