package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"relay/internal/manifest"
	"relay/internal/protocol"
	"relay/internal/protocol/rest"
	"relay/pkg/relay"
)

// cursorPaginationManifest declares a cursor strategy with every optional field
// set, so specFor's projection can be checked field by field (spec §20).
const cursorPaginationManifest = "apiVersion: relay/v1\n" +
	"kind: Tool\n" +
	"metadata:\n" +
	"  name: demo\n" +
	"  version: 1.0.0\n" +
	"runtime:\n" +
	"  name: relay\n" +
	"  apiVersion: v1\n" +
	"protocol:\n" +
	"  type: rest\n" +
	"  baseUrl: https://example.test\n" +
	"tools:\n" +
	"  - name: list\n" +
	"    description: list items\n" +
	"    input:\n" +
	"      type: object\n" +
	"    request:\n" +
	"      method: GET\n" +
	"      path: /items\n" +
	"      pagination:\n" +
	"        style: cursor\n" +
	"        cursorParam: after\n" +
	"        cursorIn: body\n" +
	"        cursorField: meta.cursor\n" +
	"        hasMoreField: meta.more\n" +
	"        limitParam: per_page\n" +
	"        limit: 50\n"

// linkPaginationManifest is a REST collection operation with a link-header
// strategy. baseURL is substituted so a test can point it at an httptest server.
func linkPaginationManifest(baseURL string) string {
	return fmt.Sprintf("apiVersion: relay/v1\n"+
		"kind: Tool\n"+
		"metadata:\n"+
		"  name: demo\n"+
		"  version: 1.0.0\n"+
		"runtime:\n"+
		"  name: relay\n"+
		"  apiVersion: v1\n"+
		"protocol:\n"+
		"  type: rest\n"+
		"  baseUrl: %s\n"+
		"tools:\n"+
		"  - name: list\n"+
		"    description: list items\n"+
		"    input:\n"+
		"      type: object\n"+
		"    request:\n"+
		"      method: GET\n"+
		"      path: /items\n"+
		"      pagination:\n"+
		"        style: link-header\n", baseURL)
}

// TestSpecForProjectsPagination proves the manifest's declared strategy reaches
// the executor's transport-neutral spec with every field intact (spec §20).
func TestSpecForProjectsPagination(t *testing.T) {
	doc, err := manifest.Parse([]byte(cursorPaginationManifest))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	operation := doc.Operation("list")
	if operation == nil {
		t.Fatal("manifest has no operation list")
	}

	spec := specFor(doc, operation)
	if spec.Pagination == nil {
		t.Fatal("spec.Pagination is nil, want the projected strategy")
	}
	want := protocol.Pagination{
		Style:        "cursor",
		CursorParam:  "after",
		CursorIn:     "body",
		CursorField:  "meta.cursor",
		HasMoreField: "meta.more",
		LimitParam:   "per_page",
		Limit:        50,
	}
	if got := *spec.Pagination; got != want {
		t.Fatalf("projected pagination = %+v, want %+v", got, want)
	}
}

// TestSpecForLeavesPaginationNilWhenAbsent proves an operation that declares no
// strategy projects nothing, so it keeps executing as a single request.
func TestSpecForLeavesPaginationNilWhenAbsent(t *testing.T) {
	doc, err := manifest.Parse([]byte(noAuthManifest))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	spec := specFor(doc, doc.Operation("ping"))
	if spec.Pagination != nil {
		t.Fatalf("spec.Pagination = %+v, want nil", spec.Pagination)
	}
}

// pageRecorder is a concurrency-safe request log for the walk tests.
type pageRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *pageRecorder) add(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
}

func (r *pageRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.paths)
}

func (r *pageRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// threePageServer serves an RFC 8288 link-header chain of three pages.
func threePageServer(t *testing.T) (*httptest.Server, *pageRecorder) {
	t.Helper()
	rec := &pageRecorder{}
	pages := map[string][]map[string]any{
		"":  {{"id": 1}, {"id": 2}},
		"2": {{"id": 3}},
		"3": {{"id": 4}, {"id": 5}},
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.RequestURI())
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "":
			w.Header().Set("Link", fmt.Sprintf("<%s/items?page=2>; rel=\"next\"", srv.URL))
		case "2":
			w.Header().Set("Link", fmt.Sprintf("<%s/items?page=3>; rel=\"next\"", srv.URL))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": pages[page]})
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestInvokeDefaultStaysSinglePage proves the flag-off path is unchanged: the
// strategy is projected into the spec but the executor still makes exactly one
// request and nothing about a walk is surfaced to the caller (spec §20).
func TestInvokeDefaultStaysSinglePage(t *testing.T) {
	srv, rec := threePageServer(t)
	d := newDaemonHarness(t, linkPaginationManifest(srv.URL), rest.New(), newMemKeychain())

	response := d.invoke(context.Background(), relay.InvokeRequest{
		Type: relay.FrameInvoke, Tool: "demo", Operation: "list", Input: map[string]any{},
	})
	if !response.Success {
		t.Fatalf("invoke failed: %v", response.Error)
	}
	if rec.count() != 1 {
		t.Fatalf("server saw %d requests, want 1: %v", rec.count(), rec.seen())
	}
	if response.Pages != 0 || response.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 0/false with pagination off", response.Pages, response.Truncated)
	}
	body, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %T", response.Result)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("default result items = %v, want the first page's two items", body["items"])
	}
}

// TestInvokePaginateWalksEveryPage drives a real walk through the daemon: the
// executor follows the declared Link chain and the aggregate is returned with
// its page count surfaced (spec §20).
func TestInvokePaginateWalksEveryPage(t *testing.T) {
	srv, rec := threePageServer(t)
	d := newDaemonHarness(t, linkPaginationManifest(srv.URL), rest.New(), newMemKeychain())

	response := d.invoke(context.Background(), relay.InvokeRequest{
		Type: relay.FrameInvoke, Tool: "demo", Operation: "list", Input: map[string]any{},
		Paginate: true,
	})
	if !response.Success {
		t.Fatalf("paginated invoke failed: %v", response.Error)
	}
	if rec.count() != 3 {
		t.Fatalf("server saw %d requests, want 3: %v", rec.count(), rec.seen())
	}
	if want := []string{"/items", "/items?page=2", "/items?page=3"}; !equalStrings(rec.seen(), want) {
		t.Fatalf("walked paths = %v, want %v", rec.seen(), want)
	}
	if response.Pages != 3 || response.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 3/false", response.Pages, response.Truncated)
	}
	body, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %T", response.Result)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 5 {
		t.Fatalf("aggregated items = %v, want 5 items across the walk", body["items"])
	}
}

// TestInvokeThreadsPaginateAndSurfacesShape pins the opt-in boundary with a fake
// executor: the flag reaches the executor, a projected strategy is present even
// when the flag is off, and Pages/Truncated reach the caller only when asked.
func TestInvokeThreadsPaginateAndSurfacesShape(t *testing.T) {
	executor := &fakeExecutor{
		response: protocol.Response{
			Status: 200, Body: map[string]any{"ok": true}, Pages: 4, Truncated: true,
		},
	}
	d := newDaemonHarness(t, linkPaginationManifest("https://example.test"), executor, newMemKeychain())

	off := d.invoke(context.Background(), relay.InvokeRequest{
		Type: relay.FrameInvoke, Tool: "demo", Operation: "list", Input: map[string]any{},
	})
	if !off.Success {
		t.Fatalf("invoke failed: %v", off.Error)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(executor.requests))
	}
	if executor.requests[0].Paginate {
		t.Fatal("the flag was off but the executor was asked to paginate")
	}
	if pag := executor.requests[0].Spec.Pagination; pag == nil || pag.Style != "link-header" {
		t.Fatalf("declared strategy was not projected: %+v", pag)
	}
	if off.Pages != 0 || off.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 0/false when the flag is off", off.Pages, off.Truncated)
	}

	on := d.invoke(context.Background(), relay.InvokeRequest{
		Type: relay.FrameInvoke, Tool: "demo", Operation: "list", Input: map[string]any{},
		Paginate: true,
	})
	if !on.Success {
		t.Fatalf("paginated invoke failed: %v", on.Error)
	}
	if !executor.requests[1].Paginate {
		t.Fatal("the flag was on but the executor was not asked to paginate")
	}
	if on.Pages != 4 || !on.Truncated {
		t.Fatalf("pages=%d truncated=%v, want the walk's 4/true", on.Pages, on.Truncated)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
