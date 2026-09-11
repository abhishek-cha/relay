package browser

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClientFollowsSameHostRedirect is the negative control for the security
// guard below: an ordinary same-host redirect must still be followed, or the
// guard would break honest services.
func TestClientFollowsSameHostRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/from", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/to", http.StatusFound)
	})
	mux.HandleFunc("/to", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := HTTPClient(nil, 5*time.Second, nil)
	response, err := client.Get(srv.URL + "/from")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after following a same-host redirect", response.StatusCode)
	}
}

// TestClientStopsCrossHostRedirect proves the session cookie never reaches a
// second host. A host-only cookie is still eligible for any port on that host,
// and a 3xx to an attacker-controlled host is how a session is exfiltrated, so
// the client must stop at the redirect rather than replay the cookie
// (spec §23, §40).
func TestClientStopsCrossHostRedirect(t *testing.T) {
	var reachedOtherHost bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedOtherHost = true
		if _, err := r.Cookie("sid"); err == nil {
			t.Errorf("cookie leaked to the redirect target")
		}
	}))
	defer other.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer origin.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	originURL := mustURL(t, origin.URL)
	jar.SetCookies(originURL, []*http.Cookie{{Name: "sid", Value: "cookie-value", Path: "/"}})

	client := HTTPClient(nil, 5*time.Second, jar)
	response, err := client.Get(origin.URL + "/go")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 returned without following", response.StatusCode)
	}
	if reachedOtherHost {
		t.Fatal("the cross-host redirect target was contacted; the cookie could have leaked")
	}
}

// TestClientStopsAfterMaxRedirects bounds a redirect loop on one host.
func TestClientStopsAfterMaxRedirects(t *testing.T) {
	var serverURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, serverURL+"/loop", http.StatusFound)
	}))
	defer srv.Close()
	serverURL = srv.URL

	client := HTTPClient(nil, 5*time.Second, nil)
	_, err := client.Get(srv.URL + "/loop")
	if err == nil {
		t.Fatal("a redirect loop should fail rather than spin forever")
	}
}
