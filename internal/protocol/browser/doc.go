// Package browser implements the BrowserExecutor behind the same
// protocol.Executor seam REST, GraphQL, gRPC, and local use, so a tool whose
// manifest declares `protocol.type: browser` reaches an agent through the
// identical daemon path — no CLI or MCP change (spec §19, §23).
//
// A browser tool is an HTTP tool that needs a logged-in session rather than a
// static token. Operations carry the same request shape as REST (method, path,
// query, headers, body); the only difference is that the outbound request is
// made with the tool's stored cookie jar instead of an injected credential.
//
// This executor is built on net/http plus net/http/cookiejar, not a headless
// browser. JavaScript execution, DOM interaction, and rendered navigation are
// deliberately out of scope until a real browser engine lands (spec §23). An
// honest session layer that logs in with a form or a header and reuses the
// resulting cookies is more useful than a stubbed engine, and the seam it fills
// is where a future engine would attach.
//
// The daemon owns the login and clear frames; this package exposes them as
// [Executor.Login] and [Executor.Clear] so the daemon can call the same object
// it registered. See internal/browser for the session store and the login
// declaration reader.
//
// See TASKS.md milestone M10.
package browser
