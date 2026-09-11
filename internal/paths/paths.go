// Package paths resolves Relay's on-disk layout under ~/.relay (spec §15).
//
// Every path derives from a single root so that tests and the end-to-end script
// can redirect the entire installation with RELAY_HOME instead of touching the
// user's real directory.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvHome overrides the Relay root directory.
const EnvHome = "RELAY_HOME"

// Layout is the resolved directory tree:
//
//	~/.relay/
//	├── bin/        symlinks so tools run from PATH (spec §38)
//	├── cache/
//	├── config/
//	├── logs/
//	├── registry/   <tool>.json discovery records (spec §15)
//	├── run/        daemon.sock, daemon.lock (spec §14)
//	└── tools/      installed binaries (spec §38)
type Layout struct {
	Root     string
	Bin      string
	Cache    string
	Config   string
	Logs     string
	Registry string
	Run      string
	Tools    string
}

// Default returns the layout for this process, honoring RELAY_HOME.
func Default() Layout {
	root := os.Getenv(EnvHome)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		root = filepath.Join(home, ".relay")
	}
	absolute, err := filepath.Abs(root)
	if err == nil {
		root = absolute
	}
	return Layout{
		Root:     root,
		Bin:      filepath.Join(root, "bin"),
		Cache:    filepath.Join(root, "cache"),
		Config:   filepath.Join(root, "config"),
		Logs:     filepath.Join(root, "logs"),
		Registry: filepath.Join(root, "registry"),
		Run:      filepath.Join(root, "run"),
		Tools:    filepath.Join(root, "tools"),
	}
}

// Socket is the daemon's Unix domain socket. A Unix socket gives Relay a local
// trust boundary without opening a TCP port (spec §14).
func (l Layout) Socket() string { return filepath.Join(l.Run, "daemon.sock") }

// Lock is the daemon's single-instance lock.
func (l Layout) Lock() string { return filepath.Join(l.Run, "daemon.lock") }

// LogFile is the daemon's log destination.
func (l Layout) LogFile() string { return filepath.Join(l.Logs, "daemon.log") }

// Record is a tool's registry file.
func (l Layout) Record(name string) string {
	return filepath.Join(l.Registry, name+".json")
}

// Binary is a tool's installed binary.
func (l Layout) Binary(name string) string {
	return filepath.Join(l.Tools, name)
}

// Shims is a tool's PATH entry.
func (l Layout) Shims(name string) string {
	return filepath.Join(l.Bin, name)
}

// Ensure creates the directory tree. The run directory is private because it
// holds the daemon socket and lock.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.Bin, l.Cache, l.Config, l.Logs, l.Registry, l.Tools} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(l.Run, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", l.Run, err)
	}
	return nil
}
