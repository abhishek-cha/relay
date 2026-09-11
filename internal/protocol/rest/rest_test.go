package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relay/internal/protocol"
	"relay/pkg/relay"
)

// noOpSleeper records sleep durations without actually sleeping.
type noOpSleeper struct {
	calls []time.Duration
}

func (s *noOpSleeper) sleep(d time.Duration) {
	s.calls = append(s.calls, d)
}

func newTestExecutor(client *http.Client, sleeper *noOpSleeper) *Executor {
	return &Executor{
		Client:      client,
		Sleeper:     sleeper.sleep,
		MaxAttempts: 3,
	}
}

func TestGETSimple(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"ok":true}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/repos"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "GET" {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/repos" {
		t.Errorf("path = %q, want /repos", gotPath)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	bodyMap, ok := resp.Body.(map[string]any)
	if !ok {
		t.Fatalf("body is not a map: %T", resp.Body)
	}
	if bodyMap["ok"] != true {
		t.Errorf("body.ok = %v, want true", bodyMap["ok"])
	}
}

func TestPOSTWithJSONBody(t *testing.T) {
	var gotMethod, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		fmt.Fprintln(w, `{"created":true}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "POST",
			Path: "/items", Body: map[string]any{"name": "test"},
		},
		Input: map[string]any{"name": "test"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if parsed["name"] != "test" {
		t.Errorf("body.name = %v, want test", parsed["name"])
	}
}

func TestPathPlaceholderEscaping(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET",
			Path: "/repos/{owner}/{repo}",
		},
		Input: map[string]any{"owner": "openai", "repo": "relay"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/repos/openai/relay" {
		t.Errorf("path = %q, want /repos/openai/relay", gotPath)
	}
}

func TestPathPlaceholderWithSpecialChars(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET",
			Path: "/search/{query}",
		},
		Input: map[string]any{"query": "hello world"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/search/hello%20world" {
		t.Errorf("path = %q, want /search/hello%%20world", gotPath)
	}
}

func TestPathPlaceholderMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not have been made")
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET",
			Path: "/repos/{owner}/{repo}",
		},
		Input: map[string]any{"owner": "openai"},
	})
	if err == nil {
		t.Fatal("expected error for missing path placeholder")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeInvalidInput {
		t.Errorf("error code = %q, want %q", rErr.Code, relay.CodeInvalidInput)
	}
}

func TestQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET",
			Path:  "/search",
			Query: map[string]string{"types": "{types}", "owner": "{owner}"},
		},
		Input: map[string]any{"types": "repos,issues"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotQuery, "types=repos%2Cissues") {
		t.Errorf("query should contain types param, got: %s", gotQuery)
	}
	if strings.Contains(gotQuery, "owner=") {
		t.Errorf("query should not contain owner param, got: %s", gotQuery)
	}
}

func TestCustomHeaders(t *testing.T) {
	var gotAccept, gotXCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		gotXCustom = r.Header.Get("X-Custom")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/test",
			Headers: map[string]string{"Accept": "application/vnd.github+json", "X-Custom": "{token}"},
		},
		Input: map[string]any{"token": "abc123"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want application/vnd.github+json", gotAccept)
	}
	if gotXCustom != "abc123" {
		t.Errorf("X-Custom = %q, want abc123", gotXCustom)
	}
}

func TestUnconsumedInputBecomesBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "POST",
			Path: "/repos/{owner}/{repo}/issues",
		},
		Input: map[string]any{"owner": "openai", "repo": "relay", "title": "Bug report", "body": "Details here"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if _, ok := parsed["owner"]; ok {
		t.Error("consumed input owner should not appear in body")
	}
	if _, ok := parsed["repo"]; ok {
		t.Error("consumed input repo should not appear in body")
	}
	if parsed["title"] != "Bug report" {
		t.Errorf("body.title = %v, want Bug report", parsed["title"])
	}
	if parsed["body"] != "Details here" {
		t.Errorf("body.body = %v, want Details here", parsed["body"])
	}
}

func TestCredentialBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec:       protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/me"},
		Credential: &protocol.Credential{Type: "bearer", Secret: "ghp_secret123", Scheme: "Bearer"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer ghp_secret123" {
		t.Errorf("Authorization = %q, want Bearer ghp_secret123", gotAuth)
	}
}

func TestCredentialCustomHeader(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec:       protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/data"},
		Credential: &protocol.Credential{Type: "api_key", Secret: "my-api-key-123", Header: "X-API-Key"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotKey != "my-api-key-123" {
		t.Errorf("X-API-Key = %q, want my-api-key-123", gotKey)
	}
}

func TestCredentialNeverInErrorString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprintln(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	secret := "super-secret-token-abc"
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec:       protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/secure"},
		Credential: &protocol.Credential{Type: "bearer", Secret: secret, Scheme: "Bearer"},
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	errStr := err.Error()
	if strings.Contains(errStr, secret) {
		t.Errorf("error string contains secret: %q", errStr)
	}
}

func TestJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"count":42,"items":[1,2,3]}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/data"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bodyMap, ok := resp.Body.(map[string]any)
	if !ok {
		t.Fatalf("body is not a map: %T -- %v", resp.Body, resp.Body)
	}
	if bodyMap["count"] != float64(42) {
		t.Errorf("body.count = %v, want 42", bodyMap["count"])
	}
}

func TestNonJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		fmt.Fprintln(w, "hello plain text")
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/text"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bodyStr, ok := resp.Body.(string)
	if !ok {
		t.Fatalf("body is not a string: %T", resp.Body)
	}
	if !strings.Contains(bodyStr, "hello plain text") {
		t.Errorf("body = %q, want it to contain hello plain text", bodyStr)
	}
}

func TestError401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprintln(w, `{"message":"unauthorized"}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/secure"},
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeAuthRequired {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeAuthRequired)
	}
	if rErr.Details == nil {
		t.Error("details should not be nil")
	}
}

func TestError403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprintln(w, `{"message":"forbidden"}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/admin"},
	})
	if err == nil {
		t.Fatal("expected error for 403")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodePermissionDenied {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodePermissionDenied)
	}
}

func TestError429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Header().Set("Retry-After", "5")
		fmt.Fprintln(w, `{"message":"rate limited"}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/api"},
	})
	if err == nil {
		t.Fatal("expected error for 429")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeRateLimited {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeRateLimited)
	}
}

func TestError500NoRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprintln(w, `{"error":"internal"}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	ex.MaxAttempts = 1
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/broken"},
	})
	if err == nil {
		t.Fatal("expected error for 500")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeRemoteError {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeRemoteError)
	}
}

func TestError404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprintln(w, `{"message":"not found"}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/missing"},
	})
	if err == nil {
		t.Fatal("expected error for 404")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeRemoteError {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeRemoteError)
	}
}

func TestTransportFailure(t *testing.T) {
	ex := newTestExecutor(&http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return nil, fmt.Errorf("connection refused")
			},
		},
	}, &noOpSleeper{})
	ex.MaxAttempts = 1
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: "http://127.0.0.1:1",
			Method: "GET", Path: "/fail",
		},
	})
	if err == nil {
		t.Fatal("expected error for transport failure")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeNetworkError {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeNetworkError)
	}
}

func TestContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	ex.MaxAttempts = 1
	_, err := ex.Execute(ctx, protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/slow"},
	})
	if err == nil {
		t.Fatal("expected error for context timeout")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeTimeout {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeTimeout)
	}
}

func TestRetryGET(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&count, 1)
		if n <= 2 {
			w.WriteHeader(500)
			fmt.Fprintln(w, `{"error":"server error"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"ok":true}`)
	}))
	defer srv.Close()

	sleeper := &noOpSleeper{}
	ex := newTestExecutor(srv.Client(), sleeper)
	ex.MaxAttempts = 3
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/retry"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	total := atomic.LoadInt64(&count)
	if total != 3 {
		t.Errorf("server saw %d requests, want 3", total)
	}
	if len(sleeper.calls) != 2 {
		t.Errorf("sleeper called %d times, want 2", len(sleeper.calls))
	}
}

func TestNoRetryPOST(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(500)
		fmt.Fprintln(w, `{"error":"server error"}`)
	}))
	defer srv.Close()

	sleeper := &noOpSleeper{}
	ex := newTestExecutor(srv.Client(), sleeper)
	ex.MaxAttempts = 3
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec:  protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "POST", Path: "/create"},
		Input: map[string]any{"name": "test"},
	})
	if err == nil {
		t.Fatal("expected error for 500")
	}
	total := atomic.LoadInt64(&count)
	if total != 1 {
		t.Errorf("POST was retried: server saw %d requests, want 1", total)
	}
	if len(sleeper.calls) != 0 {
		t.Errorf("sleeper called %d times, want 0", len(sleeper.calls))
	}
}

func TestRetry429WithRetryAfter(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&count, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			fmt.Fprintln(w, `{"error":"rate limited"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"ok":true}`)
	}))
	defer srv.Close()

	sleeper := &noOpSleeper{}
	ex := newTestExecutor(srv.Client(), sleeper)
	ex.MaxAttempts = 3
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/rate-limited"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	total := atomic.LoadInt64(&count)
	if total != 2 {
		t.Errorf("server saw %d requests, want 2", total)
	}
	if len(sleeper.calls) != 1 {
		t.Fatalf("sleeper called %d times, want 1", len(sleeper.calls))
	}
	if sleeper.calls[0] != 1*time.Second {
		t.Errorf("sleeper duration = %v, want 1s", sleeper.calls[0])
	}
}

func TestHugeResponseBodyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		huge := strings.Repeat("x", maxResponseBodyBytes+1024)
		fmt.Fprint(w, huge)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/huge"},
	})
	if err == nil {
		t.Fatal("expected error for huge response body")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeRemoteError {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeRemoteError)
	}
}

func TestDefaultMethodIsGET(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Path: "/default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "GET" {
		t.Errorf("method = %q, want GET", gotMethod)
	}
}

func TestBaseURLPrecedenceOverEndpoint(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL,
			Endpoint: "http://should-not-be-used.example.com",
			Method:   "GET", Path: "/test",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(gotHost, "should-not-be-used") {
		t.Errorf("request went to Endpoint instead of BaseURL: host = %q", gotHost)
	}
}

func TestEndpointFallback(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", Endpoint: srv.URL, Method: "GET", Path: "/fallback"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/fallback" {
		t.Errorf("path = %q, want /fallback", gotPath)
	}
}

func TestNoBaseURLEndpointReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not have been made")
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", Method: "GET", Path: "/test"},
	})
	if err == nil {
		t.Fatal("expected error for missing BaseURL and Endpoint")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeInvalidInput {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeInvalidInput)
	}
}

func TestSpecBodyWithPlaceholders(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "POST", Path: "/create",
			Body: map[string]any{"name": "{name}", "count": 42},
		},
		Input: map[string]any{"name": "hello"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	if parsed["name"] != "hello" {
		t.Errorf("body.name = %v, want hello", parsed["name"])
	}
	if parsed["count"] != float64(42) {
		t.Errorf("body.count = %v, want 42", parsed["count"])
	}
}

func TestNoBodyForGET(t *testing.T) {
	var gotContentLength string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.Header.Get("Content-Length")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec:  protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/no-body"},
		Input: map[string]any{"extra": "data"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentLength != "" && gotContentLength != "0" {
		t.Errorf("GET should not have body, Content-Length = %q", gotContentLength)
	}
}

func TestResponseHeadersCaptured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "abc-123")
		w.Header().Set("X-Rate-Limit-Remaining", "42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	resp, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/headers"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Headers == nil {
		t.Fatal("response headers should not be nil")
	}
	if got := resp.Headers["X-Request-Id"]; len(got) == 0 || got[0] != "abc-123" {
		t.Errorf("X-Request-Id = %v, want [abc-123]", got)
	}
	if got := resp.Headers["X-Rate-Limit-Remaining"]; len(got) == 0 || got[0] != "42" {
		t.Errorf("X-Rate-Limit-Remaining = %v, want [42]", got)
	}
}

func TestMethodNormalization(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec:  protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "post", Path: "/test"},
		Input: map[string]any{"key": "val"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
}

func TestRetryExhaustion(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(500)
		fmt.Fprintln(w, `{"error":"always failing"}`)
	}))
	defer srv.Close()

	sleeper := &noOpSleeper{}
	ex := newTestExecutor(srv.Client(), sleeper)
	ex.MaxAttempts = 3
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{Type: "rest", BaseURL: srv.URL, Method: "GET", Path: "/always-fail"},
	})
	if err == nil {
		t.Fatal("expected error after retry exhaustion")
	}
	var rErr *relay.Error
	if !errors.As(err, &rErr) {
		t.Fatalf("error is not *relay.Error: %T", err)
	}
	if rErr.Code != relay.CodeRemoteError {
		t.Errorf("code = %q, want %q", rErr.Code, relay.CodeRemoteError)
	}
	total := atomic.LoadInt64(&count)
	if total != 3 {
		t.Errorf("server saw %d requests, want 3", total)
	}
	if len(sleeper.calls) != 2 {
		t.Errorf("sleeper called %d times, want 2", len(sleeper.calls))
	}
}

func TestDELETENoBody(t *testing.T) {
	var gotContentLength string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.Header.Get("Content-Length")
		w.WriteHeader(204)
	}))
	defer srv.Close()

	ex := newTestExecutor(srv.Client(), &noOpSleeper{})
	_, err := ex.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type: "rest", BaseURL: srv.URL, Method: "DELETE", Path: "/items/{id}",
		},
		Input: map[string]any{"id": "123"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentLength != "" && gotContentLength != "0" {
		t.Errorf("DELETE with no unconsumed input should not have body, Content-Length = %q", gotContentLength)
	}
}
