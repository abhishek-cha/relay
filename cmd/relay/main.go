// Command relay is the developer and user CLI: build, install, inspect, run the
// daemon, and expose MCP (spec §3.1).
//
// Only the version and help paths are wired up in the scaffold; each subcommand
// is tracked in TASKS.md.
package main

import (
	"fmt"
	"os"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=1.0.0" ./cmd/relay
var version = "0.0.0-dev"

const usage = `relay — the local capability runtime for AI agents

Usage:
  relay <command> [arguments]

Commands:
  build     Build a tool binary from a manifest
  install   Install and register a tool binary
  list      List registered tools
  inspect   Show a registered tool's descriptor
  daemon    Manage the Relay daemon (start|stop|status|install)
  logs      Show daemon logs
  mcp       Run the MCP server
  auth      Manage tool credentials
  version   Print the Relay version

The manifest and CLI contracts are the product; see docs/DESIGN.md.
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println("relay " + version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		// Subcommand dispatch lands with TASKS.md milestone M1.
		fmt.Fprintf(os.Stderr, "relay: %q is not implemented yet — see TASKS.md\n", args[0])
		os.Exit(2)
	}
}
