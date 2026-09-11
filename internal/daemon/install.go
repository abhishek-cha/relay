package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"relay/internal/fsutil"
	"relay/internal/registry"
	"relay/internal/sign"
	"relay/pkg/relay"
)

// installMode is the permission of an installed tool binary: executable, and
// nothing more. Installation grants a tool no trust beyond being runnable
// (spec §40).
const installMode os.FileMode = 0o755

// register installs a tool binary into the Relay home and records it (spec §15).
//
// Installation is explicit and daemon-owned. The caller supplies a path; the
// daemon decides what that binary is by running its own --describe contract,
// rejecting anything that is not a usable, runtime-compatible Relay tool. The
// daemon never trusts a caller's claim about a binary's contents (spec §9, §35,
// §40).
//
// The registry record written here is discovery metadata only. It deliberately
// does not capture operations' schemas, because the installed binary stays
// authoritative for those and the daemon re-reads them at execution time. A
// cached schema would be the second source of truth spec §16 forbids.
func (d *Daemon) register(ctx context.Context, request relay.RegisterRequest) relay.MutationResponse {
	source, failure := d.resolveSource(request.Path)
	if failure != nil {
		return failedMutation(failure)
	}

	// The binary speaks for itself. This also enforces the manifest apiVersion,
	// kind, name safety, and runtime compatibility checks (spec §35).
	descriptor, failure := d.readDescriptor(ctx, source)
	if failure != nil {
		return failedMutation(failure)
	}

	// Trust is decided before anything is copied. A signature that fails to
	// verify, or a tool the local policy blocks, must leave no debris behind
	// (spec §48, §49).
	trust, failure := d.resolveTrust(ctx, source, descriptor)
	if failure != nil {
		return failedMutation(failure)
	}
	if trust == sign.TrustBlocked {
		return failedMutation(relay.NewError(sign.CodeToolBlocked,
			fmt.Sprintf("%s is blocked by the local trust policy", descriptor.Name)).
			WithDetails(map[string]any{"tool": descriptor.Name, "trust": string(trust)}))
	}

	if err := d.layout.Ensure(); err != nil {
		return failedMutation(relay.NewError(relay.CodeRemoteError, err.Error()))
	}

	// The manifest name, not the file name, decides identity. Installing
	// ./dist/thing that calls itself github installs github.
	target := d.layout.Binary(descriptor.Name)

	previous, previousErr := d.store.Get(descriptor.Name)
	replacing := previousErr == nil

	// Installing from the Relay home onto itself would truncate the source
	// through the copy, so the copy is skipped when source and target are one
	// file. That case is a no-op reinstall.
	if source != target {
		if _, err := fsutil.CopyFile(source, target, installMode); err != nil {
			return failedMutation(relay.NewError(relay.CodeRemoteError,
				fmt.Sprintf("install %s: %v", descriptor.Name, err)))
		}
	}

	hash, err := hashFile(target)
	if err != nil {
		return failedMutation(relay.NewError(relay.CodeRemoteError,
			fmt.Sprintf("hash the installed binary: %v", err)))
	}

	installation := relay.Installation{
		Name:        descriptor.Name,
		Version:     descriptor.Version,
		Path:        target,
		InstalledAt: d.now().UTC(),
		Runtime:     fmt.Sprintf("%s/%s", descriptor.Runtime.Name, descriptor.Runtime.APIVersion),
		APIVersion:  descriptor.APIVersion,
		Kind:        descriptor.Kind,
		Protocol:    descriptor.Protocol,
		Skill:       descriptor.Skill,
		BinaryHash:  hash,
		Operations:  operationNames(descriptor),
	}

	// An idempotent reinstall keeps its original install time, so repeating
	// 'relay install' is genuinely a no-op rather than a record that churns.
	if replacing && previous.BinaryHash == hash {
		installation.InstalledAt = previous.InstalledAt
	}

	if err := d.store.PutSigned(installation, trust); err != nil {
		return failedMutation(relay.NewError(relay.CodeRemoteError, err.Error()))
	}

	// The PATH link is a convenience, not the registration. If it cannot be
	// created the tool is still installed and reachable by absolute path, so
	// this is reported rather than rolled back (spec §38).
	message := installMessage(installation, previous, replacing)
	if err := d.expose(descriptor.Name, target); err != nil {
		message = fmt.Sprintf("%s (warning: could not link %s into %s: %v)",
			message, descriptor.Name, d.layout.Bin, err)
	}

	return relay.MutationResponse{
		Success: true,
		Tool:    descriptor.Name,
		Message: message,
		Install: &installation,
	}
}

// remove unregisters a tool and cleans up the files Relay owns for it (spec §15).
//
// Only the copied binary and the PATH link are deleted. The source the user
// installed from is never touched: Relay did not create it.
func (d *Daemon) remove(name string) relay.MutationResponse {
	if !registry.ValidName(name) {
		return failedMutation(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("invalid tool name %q", name)))
	}

	installation, err := d.store.Get(name)
	if err != nil {
		return failedMutation(unknownTool(err, name))
	}

	if err := d.store.Remove(name); err != nil {
		return failedMutation(relay.NewError(relay.CodeRemoteError, err.Error()))
	}

	// The record is gone, so anything left behind is debris rather than a
	// registered tool; cleanup failures must not resurrect the registration.
	if installation.Path != "" {
		_ = os.Remove(installation.Path)
	}
	_ = os.Remove(d.layout.Shims(name))

	return relay.MutationResponse{
		Success: true,
		Tool:    name,
		Message: fmt.Sprintf("removed %s %s", name, installation.Version),
	}
}

// resolveSource validates the path a caller asked to install.
func (d *Daemon) resolveSource(path string) (string, *relay.Error) {
	if path == "" {
		return "", relay.NewError(relay.CodeInvalidInput, "no tool path was given")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("resolve %s: %v", path, err))
	}
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", relay.NewError(relay.CodeInvalidInput,
				fmt.Sprintf("%s does not exist", absolute))
		}
		return "", relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("read %s: %v", absolute, err))
	}
	if info.IsDir() {
		return "", relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("%s is a directory; give the path of a tool binary", absolute))
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("%s is not executable", absolute))
	}
	return absolute, nil
}

// expose links an installed tool into the PATH directory so it runs by name
// (spec §38).
func (d *Daemon) expose(name, target string) error {
	if err := os.MkdirAll(d.layout.Bin, 0o755); err != nil {
		return err
	}
	link := d.layout.Shims(name)
	if current, err := os.Readlink(link); err == nil && current == target {
		return nil
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(target, link)
}

// installMessage describes what a registration actually did, so 'relay install'
// is legible when it is a no-op and when it replaced something.
func installMessage(installation, previous relay.Installation, replacing bool) string {
	switch {
	case !replacing:
		return fmt.Sprintf("installed %s %s (%d operations)",
			installation.Name, installation.Version, len(installation.Operations))
	case previous.BinaryHash == installation.BinaryHash && previous.Version == installation.Version:
		return fmt.Sprintf("%s %s is already installed",
			installation.Name, installation.Version)
	case previous.Version != installation.Version:
		return fmt.Sprintf("updated %s %s to %s",
			installation.Name, previous.Version, installation.Version)
	default:
		return fmt.Sprintf("reinstalled %s %s", installation.Name, installation.Version)
	}
}

// operationNames lists a descriptor's operations for the registry record's
// display summary.
func operationNames(descriptor relay.Descriptor) []string {
	names := make([]string, 0, len(descriptor.Tools))
	for _, summary := range descriptor.Tools {
		names = append(names, summary.Name)
	}
	return names
}

// hashFile returns the SHA-256 of a file, so a binary swapped after
// registration is visible rather than silently trusted (spec §16).
func hashFile(path string) (string, error) {
	return sign.DigestFile(path)
}

// resolveTrust decides a tool's trust level before it is installed (spec §48,
// §49).
//
// A signature is verified first and refused on any failure, because a signature
// that does not bind the binary is worse than none: it looks like evidence while
// proving nothing. Only once the artifact is either correctly signed or
// genuinely unsigned does the local policy decide the level.
func (d *Daemon) resolveTrust(ctx context.Context, source string, descriptor relay.Descriptor) (sign.TrustLevel, *relay.Error) {
	policy, err := sign.LoadPolicy(d.layout.Config)
	if err != nil {
		return "", relay.NewError(relay.CodeRemoteError, err.Error())
	}

	identity := sign.Identity{Name: descriptor.Name}
	signed := false
	if signaturePath := sign.SignaturePath(source); fileExists(signaturePath) {
		signature, err := sign.Load(signaturePath)
		if err != nil {
			return "", signatureRefused(descriptor.Name, err)
		}
		// The signed bytes are the exact bytes the tool emits, so the daemon asks
		// the tool rather than re-encoding a parsed copy.
		manifest, failure := d.run(ctx, source, "--manifest")
		if failure != nil {
			return "", failure
		}
		describe, failure := d.run(ctx, source, "--describe")
		if failure != nil {
			return "", failure
		}
		digest, err := sign.DigestFile(source)
		if err != nil {
			return "", relay.NewError(relay.CodeRemoteError,
				fmt.Sprintf("hash %s for signature verification: %v", source, err))
		}
		if err := signature.Verify(descriptor.Name, descriptor.Version, manifest, describe, digest); err != nil {
			return "", signatureRefused(descriptor.Name, err)
		}
		signed = true
		identity.Publisher = signature.Publisher
		identity.KeyFingerprint = signature.KeyFingerprint
	}

	return sign.Decide(policy, identity, signed), nil
}

// signatureRefused reports a signature that could not be trusted. The underlying
// reason is carried because it is what tells a publisher their build drifted.
func signatureRefused(name string, err error) *relay.Error {
	return relay.NewError(sign.CodeSignatureInvalid,
		fmt.Sprintf("%s: signature verification failed: %v", name, err)).
		WithDetails(map[string]any{"tool": name})
}

// fileExists reports whether path names an existing non-directory file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
