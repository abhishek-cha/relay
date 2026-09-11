// Package daemon implements relayd: the Unix-socket server that owns the
// registry, tool invocation, protocol routing, permissions, credentials, and
// telemetry.
//
// It is the trusted component. Tool binaries are treated as potentially
// untrusted, which is why the daemon re-reads a tool's manifest from the binary
// instead of trusting a cached copy, and re-validates input that a tool already
// checked (spec §3.3, §16, §18, §40).
package daemon
