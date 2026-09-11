// Package toolruntime is the public entrypoint every generated tool binary
// calls. It exists so generated code can reach the runtime without importing an
// internal package, while the implementation stays in internal/runtime.
//
// A built tool wraps this and embeds its own manifest and skill (spec §8, §13).
package toolruntime

import (
	"context"
	"fmt"
	"io"
	"os"

	"relay/internal/runtime"
	"relay/pkg/relay"
)

// Main runs a tool binary from its embedded assets and returns its exit code.
func Main(assetManifest, assetSkill []byte) int {
	return Run(context.Background(), assetManifest, assetSkill, os.Args[1:], os.Stdout, os.Stderr)
}

// Run is Main with injectable arguments, streams, and invoker, so the binary
// contract can be tested without a daemon running.
func Run(ctx context.Context, assetManifest, assetSkill []byte, args []string, stdout, stderr io.Writer) int {
	return RunWith(ctx, assetManifest, assetSkill, args, stdout, stderr, DaemonInvoker{})
}

// RunWith is Run with an explicit Invoker.
func RunWith(ctx context.Context, assetManifest, assetSkill []byte, args []string,
	stdout, stderr io.Writer, invoker runtime.Invoker) int {

	app, err := runtime.New(runtime.Assets{Manifest: assetManifest, Skill: assetSkill},
		invoker, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "relay-tool: %v\n", err)
		return runtime.ExitError
	}
	return app.Run(ctx, args)
}

// DaemonInvoker forwards operations to the Relay daemon over IPC.
//
// The transport lands with TASKS.md milestone M2. Until then every operation
// reports a retryable transport error rather than pretending to succeed.
type DaemonInvoker struct{}

// Invoke implements runtime.Invoker.
func (DaemonInvoker) Invoke(context.Context, relay.InvokeRequest) (relay.InvokeResponse, error) {
	return relay.InvokeResponse{}, relay.NewError(relay.CodeNetworkError,
		"the Relay daemon is not available; start it with 'relay daemon start'")
}
