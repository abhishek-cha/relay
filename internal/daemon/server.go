package daemon

import (
	"context"
	"fmt"
	"os"

	"relay/internal/ipc"
)

// Run serves the daemon's IPC surface until ctx is cancelled (spec §3.3, §14).
//
// It owns the single-instance guarantee: the exclusive lock is taken before the
// socket is bound, so a second daemon on the same Relay home fails cleanly with
// ipc.ErrLocked instead of racing the first one for the registry. That is a
// correctness requirement rather than a performance choice — the registry has
// exactly one writer.
//
// Signal handling deliberately lives in cmd/relayd. This function only serves
// until its context ends, which keeps it testable.
func (d *Daemon) Run(ctx context.Context, ready func(socket string)) error {
	if err := d.layout.Ensure(); err != nil {
		return err
	}

	lock, err := ipc.Acquire(d.layout.Lock())
	if err != nil {
		return err
	}
	defer lock.Release()

	server, err := ipc.Listen(d.socket)
	if err != nil {
		return err
	}
	defer func() {
		server.Close()
		// A socket left on disk is debris the next start would have to reclaim;
		// removing it keeps a clean stop visible as a clean stop.
		os.Remove(d.socket)
	}()

	server.Handler = d
	server.ErrorLog = func(err error) {
		fmt.Fprintln(d.logw, "relayd:", err)
	}

	if ready != nil {
		ready(d.socket)
	}
	return server.Serve(ctx)
}
