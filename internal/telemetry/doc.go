// Package telemetry records local, privacy-first usage events: tool, operation,
// timestamp, duration, success/failure, and the structured error code on
// failure. It never collects tokens, keys, request bodies, responses, headers,
// or personal data (spec §31, §32, §57).
//
// Telemetry exists so Relay can observe how capabilities are actually used — the
// raw material for improving skills later (spec §31). For the MVP it is local
// only: events are appended to a single JSONL file under the Relay home and are
// never sent over the network. There is deliberately no opt-in cloud path; a
// future version may add one, but privacy-first is the default by construction
// (spec §32).
//
// Recording is best-effort by contract: a telemetry failure must never fail the
// invocation it observes. Callers are expected to discard the error from
// [Recorder.Record] (see that method for the rationale).
//
// See TASKS.md milestone M9.
package telemetry
