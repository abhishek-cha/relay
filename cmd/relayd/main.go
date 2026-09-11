// Command relayd is the Relay daemon: the trusted local runtime that owns the
// registry, credentials, protocol routing, permissions, and telemetry, and
// serves tools over ~/.relay/run/daemon.sock (spec §3.3).
//
// It is started by 'relay daemon start', by a LaunchAgent, or by hand. Signal
// handling lives here so the daemon package itself stays a plain server
// (spec §39).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"relay/internal/daemon"
	"relay/internal/ipc"
	"relay/internal/manifest"
	"relay/internal/paths"
	"relay/internal/protocol"
	"relay/internal/protocol/rest"
)

var version = "0.0.0-dev"

const usage = `relayd — the Relay local capability runtime daemon

Usage:
  relayd [--socket PATH] [--log PATH]

Flags:
  --socket PATH  Unix socket to serve on (default: <relay home>/run/daemon.sock)
  --log PATH     append logs to this file instead of stderr
  --version      print the daemon version and exit

The Relay home defaults to ~/.relay and can be redirected with RELAY_HOME.

Moving the socket does not move the registry: the single-instance lock is keyed
to the Relay home rather than to the socket, because the registry has exactly one
writer. One Relay home therefore means one daemon.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("relayd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	socket := flags.String("socket", "", "Unix socket to serve on (one daemon per Relay home)")
	logPath := flags.String("log", "", "append logs to this file instead of stderr")
	showVersion := flags.Bool("version", false, "print the daemon version and exit")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("relayd " + version + " (" + manifest.APIVersion + ")")
		return 0
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "relayd: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	layout := paths.Default()

	logDestination, closeLog, err := openLog(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayd: %v\n", err)
		return 1
	}
	defer closeLog()

	// SIGTERM is how 'relay daemon stop' and launchd stop the daemon; both should
	// shut it down the same way (spec §39).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// REST is the only protocol with an executor for the MVP (spec §20). A
	// manifest asking for graphql, grpc, browser, or local is rejected with
	// PROTOCOL_ERROR rather than guessed at, so the rejection stays honest as
	// new executors land (spec §19).
	d := daemon.New(daemon.Config{
		Layout:  layout,
		Version: version,
		Socket:  *socket,
		Log:     logDestination,
		Executors: map[string]protocol.Executor{
			"rest": rest.New(),
		},
	})

	err = d.Run(ctx, func(socket string) {
		fmt.Fprintln(logDestination, "relayd", version, "listening on", socket)
	})
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		fmt.Fprintln(logDestination, "relayd stopped")
		return 0
	case errors.Is(err, ipc.ErrLocked):
		fmt.Fprintf(os.Stderr, "relayd: another Relay daemon already owns %s\n", layout.Root)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "relayd: %v\n", err)
		return 1
	}
}

// openLog resolves where daemon output goes: an explicit file (as the LaunchAgent
// and 'relay daemon start' use), or stderr when run in the foreground.
func openLog(path string) (*os.File, func(), error) {
	if path == "" {
		return os.Stderr, func() {}, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve log path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log %s: %w", absolute, err)
	}
	return file, func() { file.Close() }, nil
}
