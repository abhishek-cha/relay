package browser

import (
	"context"

	"relay/internal/ipc"
)

// This file is the narrow seam between the IPC session frames and the session
// store. It lives with the store it drives rather than in the daemon, so the
// handler and the executor share one instance and one cookie jar (spec §19,
// §23).
//
// The daemon wires it as follows, mirroring its device-flow handler:
//
//  1. decode the frame into ipc.SessionLoginRequest / ipc.SessionClearRequest;
//  2. for login, resolve the tool's installation, then read the manifest the
//     same way execution does (d.run(ctx, installation.Path, "--manifest"));
//  3. call HandleLogin(ctx, request, rawManifest) or HandleClear(request);
//  4. return the typed response as the frame reply.
//
// The raw manifest bytes are passed through rather than a parsed document
// because the login block is an additive, untyped field the schema does not
// model yet, exactly like the OAuth2 device fields (spec §21).

// HandleLogin runs the session_login frame: it executes the tool's declared
// login flow and stores the resulting session. The reply carries no cookie —
// the daemon stored it, and the store is the only place it lives (spec §22).
func (s *Sessions) HandleLogin(ctx context.Context, request ipc.SessionLoginRequest, manifestYAML []byte) ipc.SessionLoginResponse {
	response := ipc.SessionLoginResponse{Tool: request.Tool}
	if failure := s.Login(ctx, request.Tool, manifestYAML); failure != nil {
		response.Error = failure
		return response
	}
	response.Success = true
	response.LoggedIn = true
	return response
}

// HandleClear runs the session_clear frame: it forgets the tool's session.
// Clearing is idempotent, so a tool with no session still reports Cleared.
func (s *Sessions) HandleClear(request ipc.SessionClearRequest) ipc.SessionClearResponse {
	response := ipc.SessionClearResponse{Tool: request.Tool}
	if failure := s.Clear(request.Tool); failure != nil {
		response.Error = failure
		return response
	}
	response.Success = true
	response.Cleared = true
	return response
}
