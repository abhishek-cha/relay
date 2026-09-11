// Package browser provides shared session capabilities: form and header login,
// cookie jars, and per-tool session persistence, so tools declaring
// protocol.type: browser do not each ship their own session handling (spec §23).
//
// It is built on net/http and net/http/cookiejar, not a headless browser: there
// is no JavaScript or DOM engine, and the OAuth2 redirect flow is not
// implemented here.
//
// See TASKS.md milestone M10.
package browser
