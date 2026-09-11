package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"time"

	"relay/internal/manifest"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// toolOutputLimit bounds how much an installed tool may print. A tool binary is
// potentially untrusted, so an unbounded reply is a denial of service on the
// daemon rather than a curiosity (spec §40).
const toolOutputLimit = 8 << 20

// dial opens the daemon socket. It exists so Ping can probe liveness without
// depending on the IPC client.
func dial(socket string) (net.Conn, error) {
	return net.DialTimeout("unix", socket, 250*time.Millisecond)
}

// readDescriptor runs the tool's discovery contract (spec §9).
func (d *Daemon) readDescriptor(ctx context.Context, path string) (relay.Descriptor, *relay.Error) {
	output, failure := d.run(ctx, path, "--describe")
	if failure != nil {
		return relay.Descriptor{}, failure
	}
	var descriptor relay.Descriptor
	if err := json.Unmarshal(output, &descriptor); err != nil {
		return relay.Descriptor{}, relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("%s --describe did not return a Relay descriptor: %v", path, err))
	}
	if failure := validateDescriptor(descriptor, path); failure != nil {
		return relay.Descriptor{}, failure
	}
	return descriptor, nil
}

// readManifest asks the installed binary for its own manifest and checks that it
// is compatible with this daemon.
//
// This is what keeps spec §16 true in practice: the binary, not the registry, is
// the schema source, so a tool replaced in place is picked up without a
// re-registration step.
func (d *Daemon) readManifest(ctx context.Context, path string) (*manifest.Document, *relay.Error) {
	output, failure := d.run(ctx, path, "--manifest")
	if failure != nil {
		return nil, failure
	}
	doc, err := manifest.Parse(output)
	if err != nil {
		return nil, relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("%s did not return a readable manifest: %v", path, err))
	}
	if failure := d.checkRuntime(doc, path); failure != nil {
		return nil, failure
	}
	return doc, nil
}

// run executes an installed tool and returns its stdout.
func (d *Daemon) run(ctx context.Context, path string, args ...string) ([]byte, *relay.Error) {
	toolContext, cancel := context.WithTimeout(ctx, d.toolTimeout)
	defer cancel()

	stdout := &limitedBuffer{limit: toolOutputLimit}
	stderr := &limitedBuffer{limit: 64 << 10}
	command := exec.CommandContext(toolContext, path, args...)
	command.Stdout = stdout
	command.Stderr = stderr

	if err := command.Run(); err != nil {
		message := fmt.Sprintf("%s %v failed: %v", path, args, err)
		if detail := bytes.TrimSpace(stderr.Bytes()); len(detail) > 0 {
			message = fmt.Sprintf("%s: %s", message, detail)
		}
		return nil, relay.NewError(relay.CodeRuntimeIncompatible, message)
	}
	return stdout.Bytes(), nil
}

// checkRuntime enforces the compatibility declaration from spec §35. An
// incompatible tool is rejected outright rather than executed and left to fail
// in some more confusing way.
func (d *Daemon) checkRuntime(doc *manifest.Document, path string) *relay.Error {
	if doc.APIVersion != manifest.APIVersion {
		return relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("%s declares manifest apiVersion %q; this daemon speaks %q",
				path, doc.APIVersion, manifest.APIVersion))
	}
	if doc.Kind != manifest.KindTool {
		return relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("%s declares kind %q; expected %q", path, doc.Kind, manifest.KindTool))
	}
	name := doc.Runtime.Name
	if name == "" {
		name = relay.RuntimeName
	}
	if name != relay.RuntimeName {
		return relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("%s requires runtime %q; this daemon provides %q",
				path, name, relay.RuntimeName))
	}
	if !relay.CompatibleRuntimeAPIVersion(doc.Runtime.APIVersion, relay.RuntimeAPIVersion) {
		return relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("tool requires Relay runtime %s, but this daemon provides %s. Please upgrade Relay.",
				doc.Runtime.APIVersion, relay.RuntimeAPIVersion))
	}
	return nil
}

// validateDescriptor checks that a --describe reply really describes a usable
// Relay tool.
func validateDescriptor(descriptor relay.Descriptor, path string) *relay.Error {
	incompatible := func(message string) *relay.Error {
		return relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("%s is not a usable Relay tool: %s", path, message))
	}
	switch {
	case descriptor.APIVersion != manifest.APIVersion:
		return incompatible(fmt.Sprintf("it declares apiVersion %q, expected %q",
			descriptor.APIVersion, manifest.APIVersion))
	case descriptor.Kind != manifest.KindTool:
		return incompatible(fmt.Sprintf("it declares kind %q, expected %q",
			descriptor.Kind, manifest.KindTool))
	case descriptor.Name == "":
		return incompatible("it has no name")
	case !registry.ValidName(descriptor.Name):
		return incompatible(fmt.Sprintf("its name %q is not a safe tool name", descriptor.Name))
	case descriptor.Version == "":
		return incompatible("it has no version")
	case descriptor.Protocol == "":
		return incompatible("it declares no protocol")
	case len(descriptor.Tools) == 0:
		return incompatible("it declares no operations")
	}
	for _, summary := range descriptor.Tools {
		if summary.Name == "" {
			return incompatible("it declares an operation with no name")
		}
	}

	runtimeName := descriptor.Runtime.Name
	if runtimeName == "" {
		runtimeName = relay.RuntimeName
	}
	if runtimeName != relay.RuntimeName {
		return incompatible(fmt.Sprintf("it requires runtime %q, this daemon provides %q",
			runtimeName, relay.RuntimeName))
	}
	if !relay.CompatibleRuntimeAPIVersion(descriptor.Runtime.APIVersion, relay.RuntimeAPIVersion) {
		return incompatible(fmt.Sprintf(
			"it requires Relay runtime %s, but this daemon provides %s. Please upgrade Relay.",
			descriptor.Runtime.APIVersion, relay.RuntimeAPIVersion))
	}
	return nil
}

// limitedBuffer collects output up to a cap, then refuses to accept more.
type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(chunk []byte) (int, error) {
	if b.buffer.Len()+len(chunk) > b.limit {
		return 0, fmt.Errorf("output exceeds %d bytes", b.limit)
	}
	return b.buffer.Write(chunk)
}

func (b *limitedBuffer) Bytes() []byte { return b.buffer.Bytes() }
