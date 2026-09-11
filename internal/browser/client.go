package browser

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultTimeout bounds one exchange with a service.
const defaultTimeout = 30 * time.Second

// maxRedirects is the same ceiling net/http applies by default.
const maxRedirects = 10

// HTTPClient builds a client that carries one tool's session and refuses to
// follow a redirect to a different host.
//
// Refusing cross-host redirects is a security boundary, not a convenience
// (spec §23, §40). A cookie jar scopes cookies by host, but a host-only cookie
// for an IP or a bare host is still eligible for *any port* on that host, and a
// 3xx to an attacker-controlled host is exactly how a session is exfiltrated:
// the browser would replay the cookie to the second host before the service is
// ever reached. Relay stops at the redirect instead and hands the 3xx back as a
// remote failure, so no credential leaves the daemon toward a host the manifest
// did not name.
func HTTPClient(transport http.RoundTripper, timeout time.Duration, jar http.CookieJar) *http.Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Transport:     transport,
		Jar:           jar,
		Timeout:       timeout,
		CheckRedirect: sameHostRedirect,
	}
}

// sameHostRedirect permits a redirect only while it stays on the origin host.
//
// Returning [http.ErrUseLastResponse] makes the client stop and return the 3xx
// response instead of following it, so the caller reports "HTTP 302" rather
// than silently contacting another host. Host comparison includes the port:
// two services on one machine are two different endpoints.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return http.ErrUseLastResponse
	}
	return nil
}
