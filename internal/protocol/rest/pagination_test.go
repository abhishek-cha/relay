package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"relay/internal/protocol"
)

// newPaginatedExecutor builds an executor with retries off, so a pagination
// test observes exactly one request per page (spec §20).
func newPaginatedExecutor(client *http.Client) *Executor {
	ex := newTestExecutor(client, &noOpSleeper{})
	ex.MaxAttempts = 1
	return ex
}

// recorder is a concurrency-safe request log for the pagination tests.
type recorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *recorder) add(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, url)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// TestPaginationLinkHeaderChain walks a three-page RFC 8288 Link chain and
// checks the pages are concatenated without changing the result's shape.
func TestPaginationLinkHeaderChain(t *testing.T) {
	rec := &recorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=2>; rel="next"`, srv.URL))
			fmt.Fprint(w, `{"items":[{"id":1},{"id":2}]}`)
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=3>; rel="next", <%s/items?page=1>; rel="prev"`, srv.URL, srv.URL))
			fmt.Fprint(w, `{"items":[{"id":3}]}`)
		default:
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=2>; rel="prev"`, srv.URL))
			fmt.Fprint(w, `{"items":[{"id":4},{"id":5}]}`)
		}
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/items",
			Pagination: &protocol.Pagination{Style: "link-header"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.count() != 3 {
		t.Fatalf("server saw %d requests, want 3: %v", rec.count(), rec.requests)
	}
	if resp.Pages != 3 || resp.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 3/false", resp.Pages, resp.Truncated)
	}
	body, ok := resp.Body.(map[string]any)
	if !ok {
		t.Fatalf("body is not an object: %T", resp.Body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("body.items is not an array: %T", body["items"])
	}
	if len(items) != 5 {
		t.Fatalf("aggregated items = %d, want 5: %v", len(items), items)
	}
}

// TestPaginationCursorChain walks a cursor chain that ends when the response
// stops carrying a cursor, and proves the cursor is echoed into the request.
func TestPaginationCursorChain(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		rec.add(cursor)
		w.Header().Set("Content-Type", "application/json")
		if cursor == "" {
			fmt.Fprint(w, `{"results":[{"id":1}],"next_cursor":"c1"}`)
			return
		}
		fmt.Fprint(w, `{"results":[{"id":2},{"id":3}],"next_cursor":""}`)
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/search",
			Pagination: &protocol.Pagination{
				Style: "cursor", CursorParam: "cursor", CursorIn: "query", CursorField: "next_cursor",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.count() != 2 {
		t.Fatalf("server saw %d requests, want 2: %v", rec.count(), rec.requests)
	}
	if rec.requests[0] != "" || rec.requests[1] != "c1" {
		t.Fatalf("cursors seen = %v, want [\"\" c1]", rec.requests)
	}
	if resp.Pages != 2 || resp.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 2/false", resp.Pages, resp.Truncated)
	}
	body := resp.Body.(map[string]any)
	items := body["results"].([]any)
	if len(items) != 3 {
		t.Fatalf("aggregated results = %d, want 3", len(items))
	}
}

// TestPaginationCursorHitsPageCap proves the page cap stops an endless cursor
// chain and the partial result is marked truncated rather than passed off as
// the whole collection (spec §20).
func TestPaginationCursorHitsPageCap(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"results":[{"id":1}],"next_cursor":"c%d"}`, rec.count())
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	ex.MaxPages = 2
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/search",
			Pagination: &protocol.Pagination{Style: "cursor", CursorParam: "cursor", CursorField: "next_cursor"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.count() != 2 {
		t.Fatalf("server saw %d requests, want 2 (the cap)", rec.count())
	}
	if resp.Pages != 2 || !resp.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 2/true", resp.Pages, resp.Truncated)
	}
}

// TestPaginationHitsTotalByteCap proves the total-byte cap bounds a walk even
// when the page count would allow more (spec §20, §40).
func TestPaginationHitsTotalByteCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"results":[{"id":1}],"next_cursor":"c1"}`)
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	ex.MaxPages = 1000
	ex.MaxTotalBytes = 10 // smaller than one page body
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/search",
			Pagination: &protocol.Pagination{Style: "cursor", CursorParam: "cursor", CursorField: "next_cursor"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Pages != 1 || !resp.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 1/true", resp.Pages, resp.Truncated)
	}
}

// TestPaginationMissingCursorField ends the walk when the response omits the
// declared cursor field, without inventing a next page.
func TestPaginationMissingCursorField(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"id":1}]}`)
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/search",
			Pagination: &protocol.Pagination{Style: "cursor", CursorParam: "cursor", CursorField: "next_cursor"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("server saw %d requests, want 1", rec.count())
	}
	if resp.Pages != 1 || resp.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 1/false", resp.Pages, resp.Truncated)
	}
}

// TestPaginationHasMoreStopsEarly proves the optional has-more field ends the
// walk even while the cursor field still carries a value.
func TestPaginationHasMoreStopsEarly(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"id":1}],"next_cursor":"c1","has_more":false}`)
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/search",
			Pagination: &protocol.Pagination{
				Style: "cursor", CursorParam: "cursor", CursorField: "next_cursor", HasMoreField: "has_more",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("server saw %d requests, want 1", rec.count())
	}
	if resp.Pages != 1 || resp.Truncated {
		t.Fatalf("pages=%d truncated=%v, want 1/false", resp.Pages, resp.Truncated)
	}
}

// TestNoPaginationIsSingleRequest proves the unchanged path: without both a
// declared strategy and an explicit opt-in, an operation is exactly one request
// and the response keeps the raw page body (spec §20).
func TestNoPaginationIsSingleRequest(t *testing.T) {
	tests := []struct {
		name     string
		paginate bool
		declared *protocol.Pagination
	}{
		{name: "no block declared", paginate: true, declared: nil},
		{name: "declared but not requested", paginate: false, declared: &protocol.Pagination{Style: "link-header"}},
		{name: "neither", paginate: false, declared: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := &recorder{}
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.add(r.URL.RequestURI())
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Link", fmt.Sprintf(`<%s/items?page=2>; rel="next"`, srv.URL))
				fmt.Fprint(w, `[{"id":1}]`)
			}))
			defer srv.Close()

			ex := newPaginatedExecutor(srv.Client())
			resp, err := ex.Execute(context.Background(), protocol.Request{
				Paginate: test.paginate,
				Spec: protocol.Spec{
					Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/items",
					Pagination: test.declared,
				},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.count() != 1 {
				t.Fatalf("server saw %d requests, want 1", rec.count())
			}
			if resp.Pages != 1 || resp.Truncated {
				t.Fatalf("pages=%d truncated=%v, want 1/false", resp.Pages, resp.Truncated)
			}
			// The body is the raw page array, not a merged multi-page result.
			items, ok := resp.Body.([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("body = %T %v, want one array of length 1", resp.Body, resp.Body)
			}
		})
	}
}

// TestPaginationLimitParamSent pins that the declared page-size parameter is
// sent on the first request.
func TestPaginationLimitParamSent(t *testing.T) {
	var gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("per_page")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	_, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/items",
			Pagination: &protocol.Pagination{Style: "link-header", LimitParam: "per_page", Limit: 100},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotLimit != "100" {
		t.Fatalf("per_page = %q, want 100", gotLimit)
	}
}

// TestParseLinkHeader covers the RFC 8288 parsing edge cases the chain tests do
// not: multiple values, commas inside a URL, and several relation types.
func TestParseLinkHeader(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{
			name:  "next and last",
			value: `<https://api.example.com/items?page=2>; rel="next", <https://api.example.com/items?page=9>; rel="last"`,
			want:  []string{"https://api.example.com/items?page=2"},
		},
		{
			name:  "comma inside query",
			value: `<https://api.example.com/items?ids=1,2>; rel="next"`,
			want:  []string{"https://api.example.com/items?ids=1,2"},
		},
		{
			name:  "no next",
			value: `<https://api.example.com/items?page=1>; rel="prev"`,
			want:  nil,
		},
		{
			name:  "multi relation",
			value: `<https://api.example.com/items?page=2>; rel="next last"`,
			want:  []string{"https://api.example.com/items?page=2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			for _, entry := range parseLinkHeader(test.value) {
				if entry.relNext {
					got = append(got, entry.target)
				}
			}
			if strings.Join(got, "|") != strings.Join(test.want, "|") {
				t.Fatalf("next links = %v, want %v", got, test.want)
			}
		})
	}
}

// TestPaginationAggregatesObjectEnvelope pins that an envelope body keeps its
// object shape while its array member grows across pages.
func TestPaginationAggregatesObjectEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"total":2,"data":[{"id":1}],"next_cursor":"c1"}`)
			return
		}
		fmt.Fprint(w, `{"total":2,"data":[{"id":2}],"next_cursor":""}`)
	}))
	defer srv.Close()

	ex := newPaginatedExecutor(srv.Client())
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Paginate: true,
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/items",
			Pagination: &protocol.Pagination{Style: "cursor", CursorParam: "cursor", CursorField: "next_cursor"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, ok := resp.Body.(map[string]any)
	if !ok {
		t.Fatalf("body = %T, want object envelope", resp.Body)
	}
	if data, ok := body["data"].([]any); !ok || len(data) != 2 {
		t.Fatalf("body.data = %v, want two items", body["data"])
	}
	if string(mustJSON(t, body["next_cursor"])) != `""` {
		t.Fatalf("body.next_cursor = %v, want the last page's value", body["next_cursor"])
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}
