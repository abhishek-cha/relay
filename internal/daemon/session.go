package daemon

import (
	"context"
	"strings"

	"relay/internal/ipc"
	"relay/pkg/relay"
)

// This file is the daemon half of the browser-session frames (spec §23). The
// session store itself lives in internal/browser and is shared with the browser
// executor, so a login performed here is exactly the session a later operation
// uses. The handlers resolve the tool and read the manifest the same way
// execution does, then hand off to the store; nothing here sees, returns, or
// logs a cookie (spec §22).

// sessionLogin runs the session_login frame: it executes a registered tool's
// declared browser login and persists the resulting session.
//
// The raw manifest bytes are passed to the store rather than the parsed
// Document because the login block is additive and untyped, exactly like the
// OAuth2 device fields (spec §21). The reply carries no cookie: the daemon
// stored it, and the store is the only place it lives.
func (d *Daemon) sessionLogin(ctx context.Context, request ipc.SessionLoginRequest) ipc.SessionLoginResponse {
	if strings.TrimSpace(request.Tool) == "" {
		return failedSessionLogin(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	if d.sessions == nil {
		return failedSessionLogin(relay.NewError(relay.CodeProtocolError,
			"this Relay daemon has no browser session store"))
	}
	installation, err := d.store.Get(request.Tool)
	if err != nil {
		return failedSessionLogin(unknownTool(err, request.Tool))
	}
	raw, failure := d.run(ctx, installation.Path, "--manifest")
	if failure != nil {
		return failedSessionLogin(failure)
	}
	return d.sessions.HandleLogin(ctx, request, raw)
}

// sessionClear runs the session_clear frame: it forgets a tool's session.
// Clearing is idempotent, so a tool with no session is still reported cleared.
//
// Unlike a login, clearing needs no manifest: it removes the cookies the store
// owns, which is the whole of its job. The tool is looked up anyway so a
// mistyped name is a precise TOOL_NOT_FOUND rather than a silent no-op.
func (d *Daemon) sessionClear(request ipc.SessionClearRequest) ipc.SessionClearResponse {
	if strings.TrimSpace(request.Tool) == "" {
		return failedSessionClear(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	if d.sessions == nil {
		return failedSessionClear(relay.NewError(relay.CodeProtocolError,
			"this Relay daemon has no browser session store"))
	}
	if _, err := d.store.Get(request.Tool); err != nil {
		return failedSessionClear(unknownTool(err, request.Tool))
	}
	return d.sessions.HandleClear(request)
}

func failedSessionLogin(err *relay.Error) ipc.SessionLoginResponse {
	return ipc.SessionLoginResponse{Success: false, Error: err}
}

func failedSessionClear(err *relay.Error) ipc.SessionClearResponse {
	return ipc.SessionClearResponse{Success: false, Error: err}
}
