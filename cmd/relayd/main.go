// Command relayd is the Relay daemon: the trusted local runtime that owns the
// registry, credentials, protocol routing, permissions, and telemetry, and
// serves tools over ~/.relay/run/daemon.sock (spec §3.3).
//
// Scaffold only; the socket server lands with TASKS.md milestone M2.
package main

import (
	"fmt"
	"os"

	"relay/internal/manifest"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("relayd " + version + " (" + manifest.APIVersion + ")")
		return
	}
	fmt.Fprintln(os.Stderr, "relayd: not implemented yet — see TASKS.md milestone M2")
	os.Exit(1)
}
