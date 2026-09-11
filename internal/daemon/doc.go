// Package daemon implements relayd: the Unix-socket server that owns the
// registry, invocation, permissions, credentials, protocol routing, and
// telemetry. It is the trusted component; tool binaries are treated as
// potentially untrusted (spec §3.3, §18, §40).
//
// See TASKS.md milestone M2.
package daemon
