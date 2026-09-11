// Command relay-tool is the generic tool runtime that every built tool binary
// wraps.
//
// At build time a manifest and a SKILL.md are embedded into a copy of this
// runtime, producing a self-contained binary that implements the stable
// contract: --describe, --skill, --help, --version, and generated operations
// (spec §9, §13).
//
// Scaffold only; embedding and the runtime contract land with TASKS.md
// milestone M1.
package main

import (
	"fmt"
	"os"
)

// Build-time injection points. A per-tool build overrides these along with the
// embedded manifest and skill assets.
var (
	version  = "0.0.0-dev"
	toolName = "relay-tool"
)

func main() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "--version":
		fmt.Printf("%s %s\n", toolName, version)
	case len(os.Args) > 1 && os.Args[1] == "--describe":
		fmt.Fprintln(os.Stderr, "relay-tool: no manifest embedded yet — see TASKS.md milestone M1")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "relay-tool: generic runtime scaffold — see TASKS.md milestone M1")
		os.Exit(2)
	}
}
