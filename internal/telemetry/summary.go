package telemetry

import (
	"sort"
	"strings"
)

// DefaultSequenceLen is the n-gram length used for sequence mining. Three is the
// smallest window that reveals a workflow (a -> b -> c) rather than a pair, which
// is the signal the spec calls out (spec §31).
const DefaultSequenceLen = 3

// seqSep joins operations into a map key. It is a unit separator so it cannot be
// produced by a tool name and cannot collide with the operations themselves.
const seqSep = "\x1f"

// OpStats is the per-operation slice of a summary: how often it ran, how often
// it failed, and nearest-rank latency percentiles in milliseconds.
type OpStats struct {
	Count    int   `json:"count"`
	Failures int   `json:"failures"`
	P50Ms    int64 `json:"p50Ms"`
	P90Ms    int64 `json:"p90Ms"`
	P99Ms    int64 `json:"p99Ms"`
}

// Sequence is a contiguous operation n-gram observed for one tool and how many
// times it occurred (spec §31).
type Sequence struct {
	Operations []string `json:"operations"`
	Count      int      `json:"count"`
}

// ToolStats aggregates one tool’s events.
type ToolStats struct {
	Total      int                `json:"total"`
	Failures   int                `json:"failures"`
	Operations map[string]OpStats `json:"operations"`
	Sequences  []Sequence         `json:"sequences"`
}

// Summary is the deterministic rollup a future "relay stats" command renders
// (spec §31). Maps and slices are ordered so two runs over the same events
// produce identical output: map keys are sorted by encoding/json, operations
// and sequences are sorted explicitly.
type Summary struct {
	Tools map[string]ToolStats `json:"tools"`
}

// Summarize aggregates events using the default sequence length.
func Summarize(events []Event) Summary { return SummarizeN(events, DefaultSequenceLen) }

// SummarizeN aggregates events into per-tool statistics. Events are ordered by
// timestamp first (stable, so equal timestamps keep input order), which is what
// makes sequence mining meaningful when the caller’s slice is unordered.
func SummarizeN(events []Event, seqLen int) Summary {
	ordered := append([]Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})

	sum := Summary{Tools: map[string]ToolStats{}}
	durations := map[string]map[string][]int64{}
	streams := map[string][]string{}
	for _, e := range ordered {
		ts, ok := sum.Tools[e.Tool]
		if !ok {
			ts = ToolStats{Operations: map[string]OpStats{}}
		}
		ts.Total++
		if !e.Success {
			ts.Failures++
		}
		op := ts.Operations[e.Operation]
		op.Count++
		if !e.Success {
			op.Failures++
		}
		ts.Operations[e.Operation] = op
		sum.Tools[e.Tool] = ts

		if durations[e.Tool] == nil {
			durations[e.Tool] = map[string][]int64{}
		}
		durations[e.Tool][e.Operation] = append(durations[e.Tool][e.Operation], e.DurationMs)
		streams[e.Tool] = append(streams[e.Tool], e.Operation)
	}

	for tool, ts := range sum.Tools {
		for name, op := range ts.Operations {
			d := durations[tool][name]
			sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
			op.P50Ms = percentile(d, 50)
			op.P90Ms = percentile(d, 90)
			op.P99Ms = percentile(d, 99)
			ts.Operations[name] = op
		}
		ts.Sequences = extractSequences(streams[tool], seqLen)
		sum.Tools[tool] = ts
	}
	return sum
}

// extractSequences counts every contiguous n-gram in a tool’s operation stream
// and returns them most-frequent first, ties broken lexicographically so the
// result is deterministic.
func extractSequences(ops []string, n int) []Sequence {
	if n <= 0 || len(ops) < n {
		return nil
	}
	counts := map[string]int{}
	for i := 0; i+n <= len(ops); i++ {
		counts[strings.Join(ops[i:i+n], seqSep)]++
	}
	out := make([]Sequence, 0, len(counts))
	for key, count := range counts {
		out = append(out, Sequence{Operations: strings.Split(key, seqSep), Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return strings.Join(out[i].Operations, seqSep) < strings.Join(out[j].Operations, seqSep)
	})
	return out
}

// percentile returns the nearest-rank percentile of a non-negative sorted slice.
// Nearest-rank avoids interpolation so the value is always one that was actually
// observed, which keeps the number honest for small local samples.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
