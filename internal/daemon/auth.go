package daemon

import (
	"context"
	"fmt"

	"relay/internal/auth"
	"relay/internal/manifest"
	"relay/pkg/relay"
)

// authTarget resolves a tool name to its registry record and manifest.
//
// Both the status and set verbs need the declared auth type, and the manifest —
// not the registry — is machine truth for it (spec §3, §16), so it is read from
// the installed binary exactly as execution does.
func (d *Daemon) authTarget(ctx context.Context, name string) (relay.Installation, *manifest.Document, *relay.Error) {
	installation, err := d.store.Get(name)
	if err != nil {
		return relay.Installation{}, nil, unknownTool(err, name)
	}
	doc, failure := d.readManifest(ctx, installation.Path)
	if failure != nil {
		return relay.Installation{}, nil, failure
	}
	return installation, doc, nil
}

// authSet stores a credential for a registered tool (spec §21, §22).
//
// The manifest decides what the tool needs; a request that names a different
// auth type is rejected rather than silently stored under the wrong assumption.
// The secret goes straight into the Keychain and is never logged or returned.
func (d *Daemon) authSet(ctx context.Context, request relay.AuthSetRequest) relay.MutationResponse {
	if request.Tool == "" {
		return failedMutation(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, doc, failure := d.authTarget(ctx, request.Tool)
	if failure != nil {
		return failedMutation(failure)
	}
	declared, ok := auth.Declared(doc)
	if !ok {
		return failedMutation(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q declares no auth; there is no credential to store", request.Tool)))
	}
	if request.AuthType != "" && auth.Canonical(request.AuthType) != auth.Canonical(declared.Type) {
		return failedMutation(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q declares auth type %q, not %q",
				request.Tool, declared.Type, request.AuthType)))
	}
	if failure := d.resolver.Set(installation.Name, request.Secret); failure != nil {
		return failedMutation(failure)
	}
	return relay.MutationResponse{
		Success: true,
		Tool:    installation.Name,
		Message: fmt.Sprintf("stored %s credential for %s", declared.Type, installation.Name),
	}
}

// authClear removes a tool's stored credential. Clearing is idempotent: a tool
// with no stored credential is reported as cleared, because that is the state
// the caller asked for.
func (d *Daemon) authClear(request relay.AuthClearRequest) relay.MutationResponse {
	if request.Tool == "" {
		return failedMutation(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, err := d.store.Get(request.Tool)
	if err != nil {
		return failedMutation(unknownTool(err, request.Tool))
	}
	if failure := d.resolver.Clear(installation.Name); failure != nil {
		return failedMutation(failure)
	}
	return relay.MutationResponse{
		Success: true,
		Tool:    installation.Name,
		Message: fmt.Sprintf("cleared the credential for %s", installation.Name),
	}
}

// authStatus reports whether a credential is stored and what the tool declares.
//
// It reports presence as a boolean and the declared type from the manifest; the
// secret is never read out, let alone returned, so there is nothing to leak
// (spec §22).
func (d *Daemon) authStatus(ctx context.Context, request relay.AuthStatusRequest) relay.AuthStatusResponse {
	if request.Tool == "" {
		return failedAuthStatus(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, doc, failure := d.authTarget(ctx, request.Tool)
	if failure != nil {
		return failedAuthStatus(failure)
	}
	stored, failure := d.resolver.Present(installation.Name)
	if failure != nil {
		return failedAuthStatus(failure)
	}
	response := relay.AuthStatusResponse{
		Success: true,
		Tool:    installation.Name,
		Stored:  stored,
	}
	if declared, ok := auth.Declared(doc); ok {
		response.AuthType = declared.Type
		response.Provider = declared.Provider
	}
	return response
}

// failedAuthStatus mirrors the other failed* constructors for the auth status
// reply, which has its own response type.
func failedAuthStatus(err *relay.Error) relay.AuthStatusResponse {
	return relay.AuthStatusResponse{Success: false, Error: err}
}
