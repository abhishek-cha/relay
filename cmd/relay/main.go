// Command relay is the developer and user CLI: build, install, inspect, run the
// daemon, and expose MCP (spec §3.1).
//
// Subcommands land milestone by milestone; see TASKS.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"relay/internal/build"
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
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println("relay " + version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	case "build":
		return runBuild(args[1:])
	default:
		// Remaining subcommands land with later TASKS.md milestones.
		fmt.Fprintf(os.Stderr, "relay: %q is not implemented yet — see TASKS.md\n", args[0])
		return 2
	}
}

// runBuild turns a manifest into one self-contained tool binary (spec §12, §42).
func runBuild(args []string) int {
	flags := flag.NewFlagSet("relay build", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay build <manifest.yaml> [--skill SKILL.md] [--out PATH]\n")
		flags.PrintDefaults()
	}

	skillPath := flags.String("skill", "", "path to the tool's SKILL.md")
	outPath := flags.String("out", "", "output binary (default dist/<tool>)")
	sourceRoot := flags.String("source", "", "Relay source tree (default: discovered)")
	keepWork := flags.Bool("keep", false, "keep the generated build directory")

	// The Go flag package stops at the first positional argument, but
	// "relay build manifest.yaml --skill SKILL.md" reads far better than
	// forcing every flag ahead of the manifest.
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}

	result, err := build.Build(context.Background(), build.Options{
		ManifestPath: flags.Arg(0),
		SkillPath:    *skillPath,
		OutPath:      *outPath,
		SourceRoot:   *sourceRoot,
		KeepWork:     *keepWork,
		Verbose:      true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}

	// stdout carries the artifact path so scripts can capture it.
	fmt.Println(result.OutPath)
	fmt.Fprintf(os.Stderr, "relay: built %s %s\n", result.Name, result.Version)
	return 0
}

// permute reorders arguments so flags precede positionals, preserving each
// flag's value. A lone "--" ends flag parsing.
func permute(flags *flag.FlagSet, args []string) []string {
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && arg[0] == '-' {
			flagArgs = append(flagArgs, arg)
			name := strings.TrimLeft(arg, "-")
			if strings.Contains(name, "=") {
				continue // value supplied inline
			}
			flag := flags.Lookup(name)
			if flag == nil {
				continue // unknown flag: let Parse report it
			}
			if boolean, ok := flag.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
				continue
			}
			if i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return append(flagArgs, positional...)
}
