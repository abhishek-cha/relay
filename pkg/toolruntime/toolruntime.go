// Package toolruntime is the public entrypoint every generated tool binary
// calls. It exists so generated code can reach the runtime without importing an
// internal package, while the implementation stays in internal/runtime.
//
// A built tool wraps this and embeds its own manifest and skill (spec §8, §13).
package toolruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"relay/internal/ipc"
	"relay/internal/paths"
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

// DaemonInvoker forwards operations to the Relay daemon over IPC (spec §13, §14). It
// dials the daemon socket, performs the §35 runtime handshake, and sends the
// invoke request. The caller owns deciding when to retry or abort.
type DaemonInvoker struct{}

// Invoke implements runtime.Invoker. It connects to the Relay daemon, performs
// the hello handshake, sends the operation, and returns the structured response.
// Transport failures are mapped onto the structured error codes in pkg/relay
// (§26).
func (DaemonInvoker) Invoke(ctx context.Context, req relay.InvokeRequest) (relay.InvokeResponse, error) {
	layout := paths.Default()

	client, err := ipc.DialClient(layout.Socket())
	if err != nil {
		if ctx.Err() != nil {
			return relay.InvokeResponse{}, relay.NewError(relay.CodeTimeout, "operation cancelled")
		}
		// DialClient wraps its sentinels with %w, so these must be errors.Is
		// rather than == ; a direct comparison would silently never match.
		if errors.Is(err, ipc.ErrUnavailable) {
			return relay.InvokeResponse{}, relay.NewError(relay.CodeNetworkError,
				"the Relay daemon is not available; start it with 'relay daemon start'")
		}
		if errors.Is(err, ipc.ErrTimeout) {
			return relay.InvokeResponse{}, relay.NewError(relay.CodeTimeout,
				"the Relay daemon did not respond in time")
		}
		return relay.InvokeResponse{}, relay.NewError(relay.CodeNetworkError,
			fmt.Sprintf("failed to connect to Relay daemon: %v", err))
	}
	defer client.Close()

	// §35 runtime handshake: declare who we are and check compatibility.
	var hello relay.HelloResponse
	if err := client.Call(ctx, relay.HelloRequest{
		Type: relay.FrameHello,
		Client: relay.ClientInfo{
			Name:              "relay-tool",
			RuntimeAPIVersion: relay.RuntimeAPIVersion,
		},
	}, &hello); err != nil {
		return relay.InvokeResponse{}, mapTransportError(err)
	}
	if !hello.Success && hello.Error != nil {
		return relay.InvokeResponse{}, hello.Error
	}

	// Send the invoke request.
	var response relay.InvokeResponse
	if err := client.Call(ctx, req, &response); err != nil {
		return relay.InvokeResponse{}, mapTransportError(err)
	}
	return response, nil
}

// mapTransportError converts IPC transport failures into the structured error
// codes that tool binaries publish on stderr (spec §10, §26).
func mapTransportError(err error) *relay.Error {
	if errors.Is(err, ipc.ErrTimeout) {
		return relay.NewError(relay.CodeTimeout,
			"the Relay daemon did not respond in time")
	}
	if errors.Is(err, ipc.ErrUnavailable) {
		return relay.NewError(relay.CodeNetworkError,
			"the Relay daemon is not available; start it with 'relay daemon start'")
	}
	return relay.NewError(relay.CodeNetworkError,
		fmt.Sprintf("the Relay daemon is not available; start it with 'relay daemon start': %v", err))
}
