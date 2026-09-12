package daemon

import (
	"context"
	"strings"
	"time"

	"relay/internal/auth"
	"relay/internal/ipc"
	"relay/pkg/relay"
)

// Refreshing stored OAuth2 credentials — spec §21, §22, §54.
//
// The daemon owns the credential, so it is the only place a refresh token can
// live and the only speaker that can swap an expiring access token for a fresh
// one without a human. A refresh writes the rotated envelope back to the
// Keychain; no secret crosses IPC in either direction, and a skip reason or an
// error never carries a value (spec §22).
//
// The frame exists for unattended callers such as cron jobs and LaunchAgents
// (spec §54), which is why a tool with nothing to refresh is reported as a skip
// rather than an error: a scheduled job that fires a minute early or walks a
// tool that holds a pasted token must not page anyone.

// refreshSkew refreshes a little before the server would reject the token, so a
// long-running caller never races the expiry. Unknown expiry is deliberately NOT
// an expiry: StoredToken.Expiring reads an absent expiry as "not expiring",
// because a server that declared none told us nothing to schedule against.
const refreshSkew = 60 * time.Second

// refreshClient builds the refresh client. It is a seam: tests replace it when a
// real network call is not the point.
var refreshClient = func() *auth.RefreshClient {
	return auth.NewRefreshClient(auth.RefreshOptions{})
}

// authRefresh refreshes one tool's credential, or every registered tool's when
// All is set (spec §21, §54).
//
// A per-tool failure is recorded in that tool's result and never aborts the
// remaining tools: a sweep over ten tools where one server is down still
// refreshes the other nine. That is why the reply is a slice and why Success
// reflects the request — whether the daemon could answer at all — rather than
// the outcomes of the individual refreshes.
func (d *Daemon) authRefresh(ctx context.Context, request ipc.AuthRefreshRequest) ipc.AuthRefreshResponse {
	if strings.TrimSpace(request.Tool) == "" && !request.All {
		return failedAuthRefresh(relay.NewError(relay.CodeInvalidInput,
			"name a tool or pass --all"))
	}

	var installations []relay.Installation
	if request.All {
		list, err := d.store.List()
		if err != nil {
			return failedAuthRefresh(relay.NewError(relay.CodeRemoteError, err.Error()))
		}
		installations = list
	} else {
		installation, err := d.store.Get(request.Tool)
		if err != nil {
			return failedAuthRefresh(unknownTool(err, request.Tool))
		}
		installations = []relay.Installation{installation}
	}

	results := make([]ipc.AuthRefreshResult, 0, len(installations))
	for _, installation := range installations {
		results = append(results, d.refreshCredential(ctx, installation, request.Force))
	}
	return ipc.AuthRefreshResponse{Success: true, Results: results}
}

// refreshCredential runs one refresh pass for one tool and reports exactly one
// outcome: refreshed, skipped with a human-readable reason, or failed.
//
// The endpoints come from the installed binary's manifest (spec §3, §16), which
// is read the same way execution reads it. A tool that declares no OAuth2 flow
// at all, or whose stored value is not an envelope (a token a human pasted), is
// the designed no-op: it is what makes this safe to run from cron and a
// LaunchAgent without a logged-in human (spec §54).
func (d *Daemon) refreshCredential(ctx context.Context, installation relay.Installation, force bool) ipc.AuthRefreshResult {
	result := ipc.AuthRefreshResult{Tool: installation.Name}

	raw, failure := d.run(ctx, installation.Path, "--manifest")
	if failure != nil {
		result.Error = failure
		return result
	}
	spec, declared, failure := refreshSpecFromManifest(installation.Name, raw)
	if failure != nil {
		result.Error = failure
		return result
	}
	if !declared {
		result.Skipped = "the tool declares no OAuth2 authorization flow to refresh"
		return result
	}

	stored, ok, failure := d.resolver.Stored(installation.Name)
	if failure != nil {
		result.Error = failure
		return result
	}
	if !ok {
		result.Skipped = "the stored credential is not a refreshable OAuth2 token envelope"
		return result
	}
	if !force && !stored.Expiring(d.now(), refreshSkew) {
		result.Skipped = "the token is still fresh"
		return result
	}

	refreshed, failure := refreshClient().Refresh(ctx, spec, stored)
	if failure != nil {
		result.Error = failure
		return result
	}
	encoded, failure := auth.EncodeStoredToken(refreshed)
	if failure != nil {
		result.Error = failure
		return result
	}
	if failure := d.resolver.Set(installation.Name, encoded); failure != nil {
		result.Error = failure
		return result
	}
	// ExpiresAt is the only outcome detail that crosses IPC: it is metadata, and
	// the token itself stays in the Keychain (spec §22).
	result.Refreshed = true
	result.ExpiresAt = refreshed.ExpiresAt
	return result
}

// refreshSpecFromManifest extracts the token endpoint and client context a
// refresh needs. It reads the browser flow first and falls back to the device
// flow because a manifest declares at most one of them, and both share the same
// tokenEndpoint and clientId.
//
// declared is false, with no error, for a manifest that declares no OAuth2 flow
// — that is the pasted-token path and is not a failure (spec §59).
func refreshSpecFromManifest(tool string, raw []byte) (spec auth.RefreshSpec, declared bool, failure *relay.Error) {
	browserSpec, browserFlow, failure := auth.BrowserFlowSpecFromManifest(raw)
	if failure != nil {
		return auth.RefreshSpec{}, false, failure
	}
	if browserFlow {
		return auth.RefreshSpec{
			Tool:          tool,
			TokenEndpoint: browserSpec.TokenEndpoint,
			ClientID:      browserSpec.ClientID,
			Scopes:        browserSpec.Scopes,
		}, true, nil
	}

	deviceSpec, deviceFlow, failure := auth.DeviceFlowSpecFromManifest(raw)
	if failure != nil {
		return auth.RefreshSpec{}, false, failure
	}
	if deviceFlow {
		return auth.RefreshSpec{
			Tool:          tool,
			TokenEndpoint: deviceSpec.TokenEndpoint,
			ClientID:      deviceSpec.ClientID,
			Scopes:        deviceSpec.Scopes,
		}, true, nil
	}
	return auth.RefreshSpec{}, false, nil
}

// failedAuthRefresh mirrors the other failed* constructors for the refresh reply.
func failedAuthRefresh(err *relay.Error) ipc.AuthRefreshResponse {
	return ipc.AuthRefreshResponse{Success: false, Error: err}
}
