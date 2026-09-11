//go:build !unix

package ipc

import "os"

// flock degrades to a no-op on platforms without advisory locks. Single-instance
// protection then rests on the socket-existence check in Listen.
func flock(*os.File) error { return nil }

// funlock is the matching no-op.
func funlock(*os.File) error { return nil }
