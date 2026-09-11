// Package ipc implements the daemon transport: newline-delimited JSON over the
// Unix domain socket at ~/.relay/run/daemon.sock.
//
// A Unix socket gives Relay a local trust boundary without opening a TCP port
// (spec §14).
//
// See TASKS.md milestone M2.
package ipc
