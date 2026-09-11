package daemon

import (
	"time"

	"relay/internal/telemetry"
	"relay/pkg/relay"
)

// UsageRecorder records one local usage event per invocation (spec §31).
//
// The daemon depends on this one-method interface rather than on the telemetry
// package itself, so a test can observe events through a writer and the daemon
// keeps no opinion about where events are stored.
//
// A nil UsageRecorder disables telemetry. That is the default for a Daemon
// constructed directly, which keeps unit tests off the filesystem; relayd
// injects the real JSONL recorder (spec §32).
type UsageRecorder interface {
	Record(telemetry.Event) error
}

// recordUsage appends one local usage event for an invocation.
//
// The event carries the identity of the call and how it behaved, and nothing
// else: Event has no field for input values, headers, bodies, or responses, so a
// secret cannot be recorded here even by accident (spec §32).
//
// The error from Record is deliberately discarded. Telemetry observes a call; it
// must never be able to fail one, so a full disk or a rotated-away file costs an
// operator a statistic, not a result.
func (d *Daemon) recordUsage(request relay.InvokeRequest, response relay.InvokeResponse, elapsed time.Duration) {
	if d.usage == nil {
		return
	}
	event := telemetry.Event{
		Tool:       request.Tool,
		Operation:  request.Operation,
		Timestamp:  d.now(),
		DurationMs: elapsed.Milliseconds(),
		Success:    response.Success,
	}
	if response.Error != nil {
		event.ErrorCode = string(response.Error.Code)
	}
	_ = d.usage.Record(event)
}
