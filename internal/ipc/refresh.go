package ipc

import "relay/pkg/relay"

// IPC frame for refreshing a stored OAuth2 credential — spec §21, §22, §54.
//
// One frame carries both the one-tool and every-tool shapes. The daemon answers
// with a slice of per-tool results, so `relay auth refresh <tool>` is a
// one-element slice and `relay auth refresh --all` is the whole set: the CLI has
// a single rendering path, and adding a tool never needs a new frame.
//
// The frame exists for unattended callers such as cron jobs and LaunchAgents,
// which refresh on a schedule rather than on demand (spec §54). Refreshing an
// already-fresh credential is therefore a no-op that reports why it skipped
// rather than an error: a scheduled job that fires a minute early is a normal
// occurrence, not a failure, and it must not page anyone.
//
// The daemon performs the exchange and writes the rotated token to the Keychain;
// neither the refresh token, the access token, nor any other secret crosses IPC
// in either direction (spec §22). Skipped is a human-readable reason and must
// never carry a value.
const FrameAuthRefresh = "auth_refresh"

// AuthRefreshRequest asks the daemon to refresh one tool's credential, or every
// registered tool's when All is set.
//
// Tool and All are alternatives: naming a tool refreshes that tool, and All
// refreshes the whole registry for a scheduled sweep. Force refreshes even when
// the token is not near expiry, which is what an operator reaches for when a
// remote has rotated a credential out from under a still-valid token.
type AuthRefreshRequest struct {
	Type  string `json:"type"`            // always FrameAuthRefresh
	Tool  string `json:"tool,omitempty"`  // one tool
	All   bool   `json:"all,omitempty"`   // every registered tool instead
	Force bool   `json:"force,omitempty"` // refresh even when the token is not near expiry
}

// AuthRefreshResult is the outcome for one tool. On success exactly one of
// Refreshed or Skipped describes it: Skipped carries a human-readable reason
// (for example, the token is not near expiry, or the tool holds no refresh
// token) and never a value. Error carries a structured failure for just this
// tool, so one failing tool does not hide the other results in a sweep.
type AuthRefreshResult struct {
	Tool      string       `json:"tool"`
	Refreshed bool         `json:"refreshed"`
	Skipped   string       `json:"skipped,omitempty"`   // a human-readable REASON, never a value
	ExpiresAt string       `json:"expiresAt,omitempty"` // RFC3339 UTC
	Error     *relay.Error `json:"error,omitempty"`
}

// AuthRefreshResponse is the reply to FrameAuthRefresh.
//
// Results is a slice so one reply serves both the one-tool and every-tool
// request shapes. A failure that stops the whole request (no tool named, no such
// tool) is reported in Error with no results; a per-tool failure during an --all
// sweep is reported in that tool's result, so the rest of the sweep still
// reports.
type AuthRefreshResponse struct {
	Success bool                `json:"success"`
	Error   *relay.Error        `json:"error,omitempty"`
	Results []AuthRefreshResult `json:"results,omitempty"`
}
