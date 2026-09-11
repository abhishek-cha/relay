// Command relay is the developer and user CLI: build, install, inspect, run the
// daemon, and expose MCP (spec §3.1).
//
// Subcommands land milestone by milestone; see TASKS.md.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"relay/internal/build"
	"relay/internal/ipc"
	"relay/internal/mcp"
	"relay/internal/paths"
	"relay/internal/registry"
	"relay/internal/sign"
	"relay/internal/telemetry"
	"relay/pkg/relay"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=1.0.0" ./cmd/relay
var version = "0.0.0-dev"

const usage = `
relay — the local capability runtime for AI agents

Usage:
  relay <command> [arguments]

Commands:
  build     Build a tool binary from a manifest
  install   Install and register a tool binary
  keygen    Generate an ed25519 signing keypair
  sign      Sign a built tool binary so install can verify it
  list      List registered tools
  inspect   Show a registered tool's descriptor
  daemon    Manage the Relay daemon (start|stop|restart|status|install)
  logs      Show daemon logs
  mcp       Run the MCP server
  auth      Manage tool credentials
  stats     Show local usage statistics
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
	case "install":
		return runInstall(args[1:])
	case "keygen":
		return runKeygen(args[1:])
	case "sign":
		return runSign(args[1:])
	case "list":
		return runList(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "daemon":
		return runDaemon(args[1:])
	case "logs":
		return runLogs(args[1:])
	case "mcp":
		return runMCP(args[1:])
	case "auth":
		return runAuth(args[1:])
	case "stats":
		return runStats(args[1:])
	default:
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

	fmt.Println(result.OutPath)
	fmt.Fprintf(os.Stderr, "relay: built %s %s\n", result.Name, result.Version)
	return 0
}

// permute reorders arguments so flags precede positionals.
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
				continue
			}
			flag := flags.Lookup(name)
			if flag == nil {
				continue
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

// ─── install ────────────────────────────────────────────────────────────────

// runInstall registers a tool binary with the running daemon (spec §15).
func runInstall(args []string) int {
	flags := flag.NewFlagSet("relay install", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay install <path>\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}

	path := flags.Arg(0)
	absPath, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: cannot resolve path %q: %v\n", path, err)
		return 1
	}

	layout := paths.Default()
	socket := layout.Socket()
	req := relay.RegisterRequest{Type: relay.FrameRegister, Path: absPath}
	var resp relay.MutationResponse
	if err := ipc.Call(context.Background(), socket, &req, &resp); err != nil {
		if errors.Is(err, ipc.ErrUnavailable) {
			fmt.Fprintf(os.Stderr, "relay: daemon is not running; start it with 'relay daemon start'\n")
		} else {
			fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		}
		return 1
	}

	if !resp.Success {
		if resp.Error != nil {
			fmt.Fprintf(os.Stderr, "relay: %s: %s\n", resp.Error.Code, resp.Error.Message)
		} else {
			fmt.Fprintf(os.Stderr, "relay: install failed\n")
		}
		return 1
	}

	fmt.Printf("installed %s (%s)\n", resp.Tool, absPath)
	return 0
}

// ─── keygen / sign ──────────────────────────────────────────────────────────

// runKeygen generates an ed25519 signing keypair so a publisher can sign a built
// tool (spec §48).
func runKeygen(args []string) int {
	flags := flag.NewFlagSet("relay keygen", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay keygen [--out DIR] [--force]\n")
		flags.PrintDefaults()
	}
	outDir := flags.String("out", ".", "directory for the key files")
	force := flags.Bool("force", false, "overwrite existing key files")
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	privatePath := filepath.Join(*outDir, "relay-signing.key")
	publicPath := filepath.Join(*outDir, "relay-signing.pub")
	if !*force {
		for _, path := range []string{privatePath, publicPath} {
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(os.Stderr, "relay: %s already exists; pass --force to overwrite\n", path)
				return 1
			}
		}
	}

	public, private, err := sign.GenerateKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: generate key: %v\n", err)
		return 1
	}
	if err := os.WriteFile(privatePath, []byte(sign.EncodePrivateKey(private)+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "relay: write %s: %v\n", privatePath, err)
		return 1
	}
	if err := os.WriteFile(publicPath, []byte(sign.EncodePublicKey(public)+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "relay: write %s: %v\n", publicPath, err)
		return 1
	}

	// stdout is the result a caller pins in a trust policy: the key fingerprint.
	fmt.Println(sign.KeyFingerprint(public))
	fmt.Fprintf(os.Stderr, "relay: wrote %s and %s\n", privatePath, publicPath)
	return 0
}

// runSign signs a built tool binary, writing the sidecar signature that
// `relay install` verifies (spec §48).
func runSign(args []string) int {
	flags := flag.NewFlagSet("relay sign", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay sign <tool-binary> --key KEYFILE [--publisher NAME] [--out PATH]\n")
		flags.PrintDefaults()
	}
	keyPath := flags.String("key", "", "ed25519 private key written by 'relay keygen'")
	publisher := flags.String("publisher", "", "publisher label bound into the signature")
	outPath := flags.String("out", "", "signature path (default <tool-binary>.sig)")
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *keyPath == "" {
		flags.Usage()
		return 2
	}

	binary, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: cannot resolve %q: %v\n", flags.Arg(0), err)
		return 1
	}
	keyData, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: read key: %v\n", err)
		return 1
	}
	private, err := sign.ParsePrivateKey(string(keyData))
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}

	describe, err := runToolBinary(binary, "--describe")
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}
	manifest, err := runToolBinary(binary, "--manifest")
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}
	var descriptor relay.Descriptor
	if err := json.Unmarshal(describe, &descriptor); err != nil {
		fmt.Fprintf(os.Stderr, "relay: %s --describe is not a Relay descriptor: %v\n", binary, err)
		return 1
	}
	digest, err := sign.DigestFile(binary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: hash %s: %v\n", binary, err)
		return 1
	}

	signature, err := sign.Sign(private, *publisher, descriptor.Name, descriptor.Version, manifest, describe, digest, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: sign: %v\n", err)
		return 1
	}
	if *outPath == "" {
		*outPath = sign.SignaturePath(binary)
	}
	if err := sign.Save(*outPath, signature); err != nil {
		fmt.Fprintf(os.Stderr, "relay: write signature: %v\n", err)
		return 1
	}

	fmt.Println(*outPath)
	fmt.Fprintf(os.Stderr, "relay: signed %s %s as %q (key %s)\n",
		descriptor.Name, descriptor.Version, *publisher, signature.KeyFingerprint)
	return 0
}

// runToolBinary runs a built or installed tool's discovery flag and returns its
// stdout. The signature covers these exact bytes, so they are fetched from the
// binary rather than reconstructed.
func runToolBinary(binary, flagArg string) ([]byte, error) {
	output, err := exec.Command(binary, flagArg).Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", binary, flagArg, err)
	}
	return output, nil
}

// ─── list ───────────────────────────────────────────────────────────────────

// runList prints the registered tools (spec §16).
func runList(args []string) int {
	flags := flag.NewFlagSet("relay list", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay list\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	layout := paths.Default()
	socket := layout.Socket()
	req := relay.ListRequest{Type: relay.FrameList}
	var resp relay.ListResponse
	if err := ipc.Call(context.Background(), socket, &req, &resp); err != nil {
		if errors.Is(err, ipc.ErrUnavailable) {
			fmt.Fprintf(os.Stderr, "relay: daemon is not running; reading on-disk registry\n")
			return runListFromDisk(layout)
		}
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}

	if !resp.Success {
		if resp.Error != nil {
			fmt.Fprintf(os.Stderr, "relay: %s: %s\n", resp.Error.Code, resp.Error.Message)
		}
		return 1
	}

	printToolTable(resp.Tools)
	return 0
}

// runListFromDisk reads the registry directly when the daemon is not running.
func runListFromDisk(layout paths.Layout) int {
	store := registry.New(layout.Registry)
	installations, err := store.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}
	printToolTable(installations)
	return 0
}

// printToolTable prints a compact aligned table of tools to stdout.
func printToolTable(tools []relay.Installation) {
	if len(tools) == 0 {
		fmt.Println("no tools registered")
		return
	}

	nameW, versionW, protoW, opsW := 4, 7, 8, 10
	for _, t := range tools {
		if len(t.Name) > nameW {
			nameW = len(t.Name)
		}
		if len(t.Version) > versionW {
			versionW = len(t.Version)
		}
		if len(t.Protocol) > protoW {
			protoW = len(t.Protocol)
		}
		ops := strings.Join(t.Operations, ",")
		if len(ops) > opsW {
			opsW = len(ops)
		}
	}

	fmt.Printf("%-*s  %-*s  %-*s  %-*s\n", nameW, "NAME", versionW, "VERSION", protoW, "PROTOCOL", opsW, "OPERATIONS")
	for _, t := range tools {
		ops := strings.Join(t.Operations, ",")
		fmt.Printf("%-*s  %-*s  %-*s  %-*s\n", nameW, t.Name, versionW, t.Version, protoW, t.Protocol, opsW, ops)
	}
}

// ─── inspect ────────────────────────────────────────────────────────────────

// runInspect prints a tool's descriptor as indented JSON (spec §9).
func runInspect(args []string) int {
	flags := flag.NewFlagSet("relay inspect", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay inspect <tool>\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}

	tool := flags.Arg(0)
	layout := paths.Default()
	socket := layout.Socket()
	req := relay.InspectRequest{Type: relay.FrameInspect, Tool: tool}
	var resp relay.InspectResponse
	if err := ipc.Call(context.Background(), socket, &req, &resp); err != nil {
		if errors.Is(err, ipc.ErrUnavailable) {
			fmt.Fprintf(os.Stderr, "relay: daemon is not running; start it with 'relay daemon start'\n")
		} else {
			fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		}
		return 1
	}

	if !resp.Success {
		if resp.Error != nil {
			fmt.Fprintf(os.Stderr, "relay: %s: %s\n", resp.Error.Code, resp.Error.Message)
		} else {
			fmt.Fprintf(os.Stderr, "relay: inspect failed\n")
		}
		return 1
	}

	if resp.Descriptor != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp.Descriptor); err != nil {
			fmt.Fprintf(os.Stderr, "relay: encode descriptor: %v\n", err)
			return 1
		}
	}

	if resp.Install != nil {
		fmt.Fprintf(os.Stderr, "relay: installed at %s\n", resp.Install.Path)
		fmt.Fprintf(os.Stderr, "relay: version %s, protocol %s\n", resp.Install.Version, resp.Install.Protocol)
	}

	return 0
}

// ─── daemon ─────────────────────────────────────────────────────────────────

// runDaemon dispatches daemon subcommands (spec §3.3).
func runDaemon(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: relay daemon <start|stop|restart|status|install>\n")
		return 2
	}

	switch args[0] {
	case "start":
		return daemonStart()
	case "stop":
		return daemonStop()
	case "restart":
		return daemonRestart()
	case "status":
		return daemonStatus()
	case "install":
		return daemonInstall()
	default:
		fmt.Fprintf(os.Stderr, "relay: unknown daemon subcommand %q\n", args[0])
		return 2
	}
}

// daemonStart starts the relayd daemon detached (spec §3.3).
//
// If a daemon already owns this Relay home the newly spawned relayd will
// refuse to start (exit 1, "another Relay daemon already owns ..."). We
// detect this by checking the process exit status during the readiness poll:
// if the process has exited before the socket came up the start failed.
func daemonStart() int {
	layout := paths.Default()

	// Check whether the socket is already live before we do anything.
	if conn, err := net.DialTimeout("unix", layout.Socket(), 250*time.Millisecond); err == nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "relay: a daemon is already running on %s\n", layout.Socket())
		return 1
	}

	relaydPath, err := findRelayd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}

	logPath := layout.LogFile()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "relay: create log directory: %v\n", err)
		return 1
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: open log %s: %v\n", logPath, err)
		return 1
	}
	defer logFile.Close()

	cmd := exec.Command(relaydPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "relay: start daemon: %v\n", err)
		return 1
	}

	pid := cmd.Process.Pid
	fmt.Fprintf(os.Stderr, "relay: daemon starting (pid %d)\n", pid)

	// Poll the socket for readiness, but also watch for the process exiting.
	// relayd fails fast when another daemon holds the lock, so we must not
	// conflate "socket is live" with "our relayd owns it".
	socket := layout.Socket()
	deadline := time.Now().Add(3 * time.Second)
	var waitDone chan error
	if cmd.ProcessState == nil {
		waitDone = make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()
	}
	for time.Now().Before(deadline) {
		if waitDone != nil {
			select {
			case waitErr := <-waitDone:
				// relayd exited before the socket appeared.
				if waitErr != nil {
					fmt.Fprintf(os.Stderr, "relay: daemon exited: %v\n", waitErr)
				} else {
					fmt.Fprintf(os.Stderr, "relay: daemon exited unexpectedly\n")
				}
				fmt.Fprintf(os.Stderr, "relay: last lines of %s:\n", logPath)
				showLogTail(logPath, 20)
				return 1
			default:
			}
		}
		conn, err := net.DialTimeout("unix", socket, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			fmt.Fprintf(os.Stderr, "relay: daemon started (pid %d)\n", pid)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Fprintf(os.Stderr, "relay: daemon failed to start within timeout; last lines of %s:\n", logPath)
	showLogTail(logPath, 20)
	return 1
}

// findRelayd locates the relayd binary next to os.Executable() or on PATH.
func findRelayd() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "relayd")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}

	if path, err := exec.LookPath("relayd"); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("relayd binary not found; build it with 'go build ./cmd/relayd'")
}

// daemonStop stops the running daemon by sending SIGTERM (spec §3.9).
func daemonStop() int {
	layout := paths.Default()
	socket := layout.Socket()

	req := relay.StatusRequest{Type: relay.FrameStatus}
	var resp relay.StatusResponse
	if err := ipc.Call(context.Background(), socket, &req, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "relay: daemon is not running\n")
		return 0
	}

	if !resp.Success {
		fmt.Fprintf(os.Stderr, "relay: daemon is not running\n")
		return 0
	}

	pid := resp.Server.Pid
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: cannot find process %d: %v\n", pid, err)
		return 1
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "relay: send SIGTERM to %d: %v\n", pid, err)
		return 1
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socket, 250*time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "relay: daemon stopped\n")
			return 0
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Fprintf(os.Stderr, "relay: daemon did not stop within timeout\n")
	return 1
}

// daemonRestart stops then starts the daemon.
func daemonRestart() int {
	daemonStop()
	return daemonStart()
}

// daemonStatus prints daemon health information as JSON on stdout (spec §3.3).
//
// The StatusResponse is emitted as-is so scripts can parse it. Human-readable
// statusOutput is the flattened JSON shape for "relay daemon status" stdout.
// It promotes fields from ServerInfo so scripts can read d["pid"] and
// d["version"] without digging into d["server"] (spec §3.3).
type statusOutput struct {
	Success       bool    `json:"success"`
	Version       string  `json:"version"`
	Pid           int     `json:"pid"`
	Socket        string  `json:"socket"`
	StartedAt     string  `json:"startedAt"`
	RuntimeName   string  `json:"runtimeName"`
	RuntimeAPI    string  `json:"runtimeApiVersion"`
	Tools         int     `json:"tools"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

// daemonStatus prints daemon health information as JSON on stdout (spec §3.3).
//
// Human-readable diagnostics go to stderr.
func daemonStatus() int {
	layout := paths.Default()
	socket := layout.Socket()
	req := relay.StatusRequest{Type: relay.FrameStatus}
	var resp relay.StatusResponse
	if err := ipc.Call(context.Background(), socket, &req, &resp); err != nil {
		if errors.Is(err, ipc.ErrUnavailable) {
			fmt.Fprintf(os.Stderr, "relay: daemon is not running\n")
		} else {
			fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		}
		return 1
	}

	if !resp.Success {
		if resp.Error != nil {
			fmt.Fprintf(os.Stderr, "relay: %s: %s\n", resp.Error.Code, resp.Error.Message)
		} else {
			fmt.Fprintf(os.Stderr, "relay: daemon status failed\n")
		}
		return 1
	}

	// Flatten so scripts can read d["pid"], d["version"], etc.
	out := statusOutput{
		Success:       resp.Success,
		Version:       resp.Server.Version,
		Pid:           resp.Server.Pid,
		Socket:        resp.Server.Socket,
		StartedAt:     resp.Server.StartedAt.Format(time.RFC3339),
		RuntimeName:   resp.Runtime.Name,
		RuntimeAPI:    resp.Runtime.APIVersion,
		Tools:         resp.Tools,
		UptimeSeconds: resp.UptimeSeconds,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&out); err != nil {
		fmt.Fprintf(os.Stderr, "relay: encode status: %v\n", err)
		return 1
	}

	// stderr carries a human summary.
	duration := time.Duration(resp.UptimeSeconds * float64(time.Second)).Round(time.Second)
	fmt.Fprintf(os.Stderr, "relay: daemon running (pid %d, uptime %s, %d tools)\n", resp.Server.Pid, duration, resp.Tools)
	return 0
}

// daemonInstall writes a macOS LaunchAgent plist for the daemon.
func daemonInstall() int {
	layout := paths.Default()

	relaydPath, err := findRelayd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}

	absRelayd, err := filepath.Abs(relaydPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: resolve relayd path: %v\n", err)
		return 1
	}

	launchAgentsDir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "relay: create LaunchAgents directory: %v\n", err)
		return 1
	}

	plistPath := filepath.Join(launchAgentsDir, "com.relay.daemon.plist")
	logPath := layout.LogFile()
	socketPath := layout.Socket()

	plist := buildPlist(absRelayd, socketPath, logPath, layout.Root)

	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "relay: write plist: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "relay: wrote %s\n", plistPath)
	fmt.Fprintf(os.Stderr, "relay: load the daemon with:\n")
	fmt.Fprintf(os.Stderr, "  launchctl load %s\n", plistPath)
	return 0
}

// buildPlist generates the LaunchAgent plist XML.
func buildPlist(relayd, socket, log, home string) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n")
	b.WriteString("<dict>\n")
	b.WriteString("\t<key>Label</key>\n")
	b.WriteString("\t<string>com.relay.daemon</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	b.WriteString(fmt.Sprintf("\t\t<string>%s</string>\n", relayd))
	b.WriteString("\t\t<string>--socket</string>\n")
	b.WriteString(fmt.Sprintf("\t\t<string>%s</string>\n", socket))
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n")
	b.WriteString("\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n")
	b.WriteString("\t<true/>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n")
	b.WriteString(fmt.Sprintf("\t<string>%s</string>\n", log))
	b.WriteString("\t<key>StandardErrorPath</key>\n")
	b.WriteString(fmt.Sprintf("\t<string>%s</string>\n", log))
	b.WriteString("\t<key>EnvironmentVariables</key>\n")
	b.WriteString("\t<dict>\n")
	b.WriteString("\t\t<key>RELAY_HOME</key>\n")
	b.WriteString(fmt.Sprintf("\t\t<string>%s</string>\n", home))
	b.WriteString("\t</dict>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String()
}

// ─── logs ───────────────────────────────────────────────────────────────────

// runStats renders the local usage summary (spec §31, §57).
//
// The summary is computed from the JSONL stream under the Relay home, so it
// needs no daemon and works while the daemon is stopped, exactly like relay
// list. Nothing here reaches the network: telemetry stays local (spec §32).
func runStats(args []string) int {
	flags := flag.NewFlagSet("relay stats", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay stats [--json] [--tool NAME] [--sequences N]\n")
		flags.PrintDefaults()
	}
	asJSON := flags.Bool("json", false, "emit the summary as JSON")
	tool := flags.String("tool", "", "limit the summary to one tool")
	seqLen := flags.Int("sequences", telemetry.DefaultSequenceLen, "operation-sequence length to mine")
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	if *seqLen < 1 {
		fmt.Fprintln(os.Stderr, "relay: --sequences must be at least 1")
		return 2
	}

	layout := paths.Default()
	fmt.Fprintf(os.Stderr, "relay: %s\n", telemetry.File(layout))

	events, err := readTelemetry(layout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}
	if len(events) == 0 {
		fmt.Fprintln(os.Stderr, "relay: no usage recorded yet; run a tool and try again")
		return 0
	}

	summary := telemetry.SummarizeN(events, *seqLen)
	if *tool != "" {
		stats, known := summary.Tools[*tool]
		if !known {
			fmt.Fprintf(os.Stderr, "relay: no usage recorded for %q\n", *tool)
			return 1
		}
		summary = telemetry.Summary{Tools: map[string]telemetry.ToolStats{*tool: stats}}
	}

	if *asJSON {
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "relay: %v\n", err)
			return 1
		}
		fmt.Println(string(encoded))
		return 0
	}

	printSummary(os.Stdout, summary)
	return 0
}

// readTelemetry reads the local usage stream, oldest file first so observed
// sequences stay in order across a rotation. A missing file is not an error: it
// only means nothing has been recorded yet.
func readTelemetry(layout paths.Layout) ([]telemetry.Event, error) {
	var events []telemetry.Event
	for _, path := range []string{telemetry.Backup(layout), telemetry.File(layout)} {
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		read, readErr := telemetry.ReadEvents(file)
		file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		events = append(events, read...)
	}
	return events, nil
}

// printSummary renders the human view: per tool, its operations by frequency
// with failure count and latency, then the observed sequences. The sequences are
// the raw material a skill revision would be based on, and they are shown to the
// operator rather than applied, because a skill change is reviewed, never
// automatic (spec §31, §33).
func printSummary(w io.Writer, summary telemetry.Summary) {
	names := make([]string, 0, len(summary.Tools))
	for name := range summary.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		stats := summary.Tools[name]
		fmt.Fprintf(w, "%s  %d call(s), %d failure(s)\n", name, stats.Total, stats.Failures)

		operations := make([]string, 0, len(stats.Operations))
		for operation := range stats.Operations {
			operations = append(operations, operation)
		}
		sort.Slice(operations, func(i, j int) bool {
			left, right := stats.Operations[operations[i]], stats.Operations[operations[j]]
			if left.Count != right.Count {
				return left.Count > right.Count
			}
			return operations[i] < operations[j]
		})
		for _, operation := range operations {
			op := stats.Operations[operation]
			fmt.Fprintf(w, "  %-28s %4d call(s)  %3d failure(s)  p50 %5dms  p90 %5dms  p99 %5dms\n",
				operation, op.Count, op.Failures, op.P50Ms, op.P90Ms, op.P99Ms)
		}
		for _, sequence := range stats.Sequences {
			fmt.Fprintf(w, "  sequence (%dx): %s\n", sequence.Count, strings.Join(sequence.Operations, " -> "))
		}
	}
}

// runLogs prints daemon log lines (spec §3.3).
func runLogs(args []string) int {
	flags := flag.NewFlagSet("relay logs", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay logs [-n LINES]\n")
		flags.PrintDefaults()
	}
	n := flags.Int("n", 50, "number of lines to show")
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	layout := paths.Default()
	logPath := layout.LogFile()

	fmt.Fprintf(os.Stderr, "relay: %s\n", logPath)

	if err := showLogTail(logPath, *n); err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}
	return 0
}

// showLogTail prints the last n lines of a log file to stdout.
func showLogTail(path string, n int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		fmt.Println(line)
	}
	return nil
}

// ─── mcp ────────────────────────────────────────────────────────────────────

// runMCP serves the registered tools to an MCP client over stdio (spec §27).
//
// stdout is the JSON-RPC channel and carries protocol frames only; every
// diagnostic goes to stderr or --log, exactly as the tool binaries keep results
// off their diagnostics stream (spec §10).
func runMCP(args []string) int {
	flags := flag.NewFlagSet("relay mcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay mcp [--log PATH]\n")
		flags.PrintDefaults()
	}
	logPath := flags.String("log", "", "append MCP diagnostics to this file (default: stderr)")

	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	diagnostics := io.Writer(os.Stderr)
	if *logPath != "" {
		file, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "relay: open log %s: %v\n", *logPath, err)
			return 1
		}
		defer file.Close()
		diagnostics = file
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mcp.Run(ctx, paths.Default(), version, os.Stdin, os.Stdout, diagnostics); err != nil {
		fmt.Fprintf(os.Stderr, "relay: mcp: %v\n", err)
		return 1
	}
	return 0
}

// ─── auth ───────────────────────────────────────────────────────────────────

// runAuth dispatches credential management (spec §21, §54).
//
// The CLI never stores a credential itself: it hands the secret to the daemon
// once, and the daemon is what writes Keychain. Nothing here reports a secret
// back, so a credential cannot leak through this command (spec §22, §40).
func runAuth(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: relay auth <login|logout|status> <tool>\n")
		return 2
	}
	switch args[0] {
	case "login":
		return runAuthLogin(args[1:])
	case "logout":
		return runAuthLogout(args[1:])
	case "status":
		return runAuthStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "relay auth: unknown subcommand %q\n", args[0])
		return 2
	}
}

// runAuthLogin stores a secret for a tool.
//
// The secret is read from the terminal with echo disabled, or from stdin when
// stdin is not a terminal, so an unattended setup never has to put a credential
// in argv where ps can see it. There is deliberately no --secret flag for that
// reason, matching the argv caveat documented in internal/keychain.
func runAuthLogin(args []string) int {
	flags := flag.NewFlagSet("relay auth login", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: relay auth login <tool> [--type TYPE]\n")
		flags.PrintDefaults()
	}
	typeOverride := flags.String("type", "", "override the credential type declared by the manifest")

	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	tool := flags.Arg(0)

	status, code := authStatus(tool)
	if code != 0 {
		return code
	}

	// The manifest is authoritative for what the tool needs, so the declared
	// type wins unless the caller overrides it (spec §3, §21).
	authType := *typeOverride
	if authType == "" {
		authType = status.AuthType
	}
	if authType == "" {
		fmt.Fprintf(os.Stderr, "relay: %s declares no auth; pass --type if the service still needs a credential\n", tool)
		return 2
	}

	// An oauth2 tool whose manifest declares the device endpoints logs in
	// without the user ever handling a token (spec §21, §54).
	if authType == "oauth2" {
		if code, handled := runDeviceLogin(tool); handled {
			return code
		}
	}

	secret, err := readSecret(fmt.Sprintf("Secret for %s (%s): ", tool, authType))
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1
	}
	if secret == "" {
		fmt.Fprintf(os.Stderr, "relay: empty secret; nothing stored\n")
		return 1
	}

	layout := paths.Default()
	req := relay.AuthSetRequest{Type: relay.FrameAuthSet, Tool: tool, AuthType: authType, Secret: secret}
	var resp relay.MutationResponse
	if err := ipc.Call(context.Background(), layout.Socket(), &req, &resp); err != nil {
		return reportIPCError(err)
	}
	if !resp.Success {
		return reportResponseError(resp.Error, "store credential")
	}
	fmt.Printf("stored %s credential for %s\n", authType, tool)
	return 0
}

// runDeviceLogin runs the OAuth2 device authorization grant when the tool's
// manifest declares one (spec §21, §54).
//
// The daemon owns the whole exchange: it holds the device code, polls the
// authorization server, and writes the token straight to the Keychain. This
// function only shows the user the code and where to type it, so no token ever
// reaches the CLI (spec §22).
//
// It reports handled=false for the one case that is not a failure: an oauth2
// tool that declares no device endpoints, which keeps the pasted-token path
// that has always worked. Any other reason the flow cannot run also falls back,
// with the diagnostic on stderr, so a manifest that predates this feature is
// never worse off than before.
func runDeviceLogin(tool string) (int, bool) {
	layout := paths.Default()
	startRequest := ipc.AuthDeviceStartRequest{Type: ipc.FrameAuthDeviceStart, Tool: tool}
	var start ipc.AuthDeviceStartResponse
	if err := ipc.Call(context.Background(), layout.Socket(), &startRequest, &start); err != nil {
		return reportIPCError(err), true
	}
	if !start.Success {
		if start.Error == nil || start.Error.Details["deviceFlow"] != false {
			fmt.Fprintf(os.Stderr, "relay: device login unavailable (%s); falling back to a pasted token\n",
				deviceFlowReason(start.Error))
		}
		return 0, false
	}

	if start.VerificationURIComplete != "" {
		fmt.Printf("Open %s and confirm the code %s\n", start.VerificationURIComplete, start.UserCode)
	} else if start.VerificationURI != "" {
		fmt.Printf("Open %s and enter the code %s\n", start.VerificationURI, start.UserCode)
	}
	if start.ExpiresIn > 0 {
		fmt.Printf("The code expires in %d seconds; waiting for authorization...\n", start.ExpiresIn)
	}

	// The daemon polls until the human finishes, so the wait is bounded by the
	// code's own expiry rather than the default exchange timeout.
	timeout := time.Duration(start.ExpiresIn+30) * time.Second
	waitRequest := ipc.AuthDeviceWaitRequest{Type: ipc.FrameAuthDeviceWait, Flow: start.Flow}
	var wait ipc.AuthDeviceWaitResponse
	if err := ipc.CallWithTimeout(context.Background(), layout.Socket(), timeout, &waitRequest, &wait); err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return 1, true
	}
	if !wait.Success {
		return reportResponseError(wait.Error, "complete the device login"), true
	}
	fmt.Printf("stored oauth2 credential for %s\n", tool)
	return 0, true
}

// deviceFlowReason renders the daemon's reason for declining a device login
// without ever echoing a value back: only the code is printed.
func deviceFlowReason(structured *relay.Error) string {
	if structured == nil {
		return "no detail"
	}
	return string(structured.Code)
}

func runAuthLogout(args []string) int {
	flags := flag.NewFlagSet("relay auth logout", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, "usage: relay auth logout <tool>\n") }
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	tool := flags.Arg(0)

	layout := paths.Default()
	req := relay.AuthClearRequest{Type: relay.FrameAuthClear, Tool: tool}
	var resp relay.MutationResponse
	if err := ipc.Call(context.Background(), layout.Socket(), &req, &resp); err != nil {
		return reportIPCError(err)
	}
	if !resp.Success {
		return reportResponseError(resp.Error, "remove credential")
	}
	fmt.Printf("removed credential for %s\n", tool)
	return 0
}

// runAuthStatus reports whether a credential exists and what the tool declares.
func runAuthStatus(args []string) int {
	flags := flag.NewFlagSet("relay auth status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, "usage: relay auth status <tool> [--json]\n") }
	jsonOut := flags.Bool("json", false, "print the status as JSON on stdout")
	if err := flags.Parse(permute(flags, args)); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	tool := flags.Arg(0)

	status, code := authStatus(tool)
	if code != 0 {
		return code
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			fmt.Fprintf(os.Stderr, "relay: encode status: %v\n", err)
			return 1
		}
		return 0
	}

	state := "not stored"
	if status.Stored {
		state = "stored"
	}
	summary := tool + ": " + state
	if status.AuthType != "" {
		summary += " (" + status.AuthType + ")"
	}
	fmt.Println(summary)
	return 0
}

// authStatus asks the daemon for a tool's credential state.
func authStatus(tool string) (relay.AuthStatusResponse, int) {
	layout := paths.Default()
	req := relay.AuthStatusRequest{Type: relay.FrameAuthStatus, Tool: tool}
	var resp relay.AuthStatusResponse
	if err := ipc.Call(context.Background(), layout.Socket(), &req, &resp); err != nil {
		return resp, reportIPCError(err)
	}
	if !resp.Success {
		return resp, reportResponseError(resp.Error, "read credential status")
	}
	return resp, 0
}

// reportIPCError maps a transport failure to an exit code and a message.
func reportIPCError(err error) int {
	if errors.Is(err, ipc.ErrUnavailable) {
		fmt.Fprintf(os.Stderr, "relay: daemon is not running; start it with 'relay daemon start'\n")
	} else {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
	}
	return 1
}

// reportResponseError prints a structured daemon error the way the rest of the
// CLI does: code first, so the failure is recognizable (spec §26).
func reportResponseError(structured *relay.Error, action string) int {
	if structured != nil {
		fmt.Fprintf(os.Stderr, "relay: %s: %s\n", structured.Code, structured.Message)
	} else {
		fmt.Fprintf(os.Stderr, "relay: %s failed\n", action)
	}
	return 1
}

// readSecret collects a credential without echoing it.
//
// A terminal read disables echo around the prompt; a piped stdin is read as one
// line so automation never needs a TTY. Passing the secret through argv is
// deliberately unsupported (spec §22).
func readSecret(prompt string) (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}

	interactive := info.Mode()&os.ModeCharDevice != 0
	if interactive {
		fmt.Fprint(os.Stderr, prompt)
		restore, err := echoOff()
		if err != nil {
			return "", err
		}
		defer restore()
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if interactive {
		fmt.Fprintln(os.Stderr)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// echoOff disables terminal echo for the duration of a secret read and returns
// the restore function. stty is the dependency-free way to do this on macOS
// without pulling in a terminal library.
func echoOff() (func(), error) {
	config := exec.Command("stty", "-echo")
	config.Stdin = os.Stdin
	if err := config.Run(); err != nil {
		return nil, fmt.Errorf("disable terminal echo: %w", err)
	}
	return func() {
		restore := exec.Command("stty", "echo")
		restore.Stdin = os.Stdin
		_ = restore.Run()
	}, nil
}
