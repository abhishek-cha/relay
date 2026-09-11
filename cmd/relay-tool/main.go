// Command relay-tool is the generic runtime without embedded assets. It exists
// as a development reference: a real tool is this runtime plus an embedded
// manifest and SKILL.md.
//
// Build one with:
//
//	relay build examples/github/github.yaml --skill examples/github/SKILL.md
//
// The runtime contract is --describe, --skill, --help, --version, and the
// generated operations (spec §9, §13).
package main

import (
	"fmt"
	"os"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("relay-tool " + version)
		return
	}
	fmt.Fprintln(os.Stderr, "relay-tool: this is the generic runtime with no embedded assets.")
	fmt.Fprintln(os.Stderr, "Build a real tool with: relay build <manifest.yaml> --skill SKILL.md")
	os.Exit(2)
}
