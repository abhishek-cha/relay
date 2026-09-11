package telemetry

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestSummarizeFrequencies(t *testing.T) {
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	events := []Event{
		{Tool: "github", Operation: "get_repository", Timestamp: base.Add(1 * time.Second), DurationMs: 10, Success: true},
		{Tool: "github", Operation: "get_repository", Timestamp: base.Add(2 * time.Second), DurationMs: 30, Success: true},
		{Tool: "github", Operation: "list_pull_requests", Timestamp: base.Add(3 * time.Second), DurationMs: 20, Success: true},
		{Tool: "github", Operation: "list_pull_requests", Timestamp: base.Add(4 * time.Second), DurationMs: 40, Success: false, ErrorCode: "RATE_LIMITED"},
		{Tool: "slack", Operation: "post_message", Timestamp: base.Add(5 * time.Second), DurationMs: 5, Success: true},
	}
	got := Summarize(events)
	gh, ok := got.Tools["github"]
	if !ok {
		t.Fatal("missing github stats")
	}
	if gh.Total != 4 || gh.Failures != 1 {
		t.Errorf("github total/failures = %d/%d, want 4/1", gh.Total, gh.Failures)
	}
	if op := gh.Operations["get_repository"]; op.Count != 2 || op.Failures != 0 {
		t.Errorf("get_repository = %+v, want count 2 failures 0", op)
	}
	if op := gh.Operations["list_pull_requests"]; op.Count != 2 || op.Failures != 1 {
		t.Errorf("list_pull_requests = %+v, want count 2 failures 1", op)
	}
	if sl := got.Tools["slack"]; sl.Total != 1 || sl.Failures != 0 {
		t.Errorf("slack = %+v, want total 1 failures 0", sl)
	}
}

func TestSummarizePercentiles(t *testing.T) {
	tests := []struct {
		name      string
		durations []int64
		p50       int64
		p90       int64
		p99       int64
	}{
		{"single", []int64{7}, 7, 7, 7},
		{"five", []int64{10, 20, 30, 40, 50}, 30, 50, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
			var events []Event
			for i, d := range tt.durations {
				events = append(events, Event{Tool: "t", Operation: "o", Timestamp: base.Add(time.Duration(i) * time.Second), DurationMs: d, Success: true})
			}
			op := Summarize(events).Tools["t"].Operations["o"]
			if op.P50Ms != tt.p50 || op.P90Ms != tt.p90 || op.P99Ms != tt.p99 {
				t.Errorf("percentiles = %d/%d/%d, want %d/%d/%d", op.P50Ms, op.P90Ms, op.P99Ms, tt.p50, tt.p90, tt.p99)
			}
		})
	}
}

func TestExtractSequences(t *testing.T) {
	tests := []struct {
		name string
		ops  []string
		n    int
		want []Sequence
	}{
		{"exact-trigram", []string{"a", "b", "c"}, 3, []Sequence{{Operations: []string{"a", "b", "c"}, Count: 1}}},
		{
			"repeated-windows-ranked",
			[]string{"a", "b", "c", "a", "b", "c"},
			3,
			[]Sequence{
				{Operations: []string{"a", "b", "c"}, Count: 2},
				{Operations: []string{"b", "c", "a"}, Count: 1},
				{Operations: []string{"c", "a", "b"}, Count: 1},
			},
		},
		{"shorter-than-n", []string{"a", "b"}, 3, nil},
		{"zero-n", []string{"a", "b", "c"}, 0, nil},
		{
			"spec-example-n4",
			[]string{"get_repository", "list_pull_requests", "get_pull_request", "list_files"},
			4,
			[]Sequence{{Operations: []string{"get_repository", "list_pull_requests", "get_pull_request", "list_files"}, Count: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSequences(tt.ops, tt.n)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSummarizeSequencesArePerToolAndOrdered(t *testing.T) {
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	// Deliberately out of order in the slice; Summarize must order by timestamp
	// and mine each tool independently.
	events := []Event{
		{Tool: "github", Operation: "c", Timestamp: base.Add(3 * time.Second), Success: true},
		{Tool: "slack", Operation: "x", Timestamp: base.Add(1 * time.Second), Success: true},
		{Tool: "github", Operation: "a", Timestamp: base.Add(1 * time.Second), Success: true},
		{Tool: "slack", Operation: "y", Timestamp: base.Add(2 * time.Second), Success: true},
		{Tool: "github", Operation: "b", Timestamp: base.Add(2 * time.Second), Success: true},
		{Tool: "slack", Operation: "z", Timestamp: base.Add(3 * time.Second), Success: true},
	}
	got := Summarize(events)
	wantGitHub := []Sequence{{Operations: []string{"a", "b", "c"}, Count: 1}}
	if got := got.Tools["github"].Sequences; !reflect.DeepEqual(got, wantGitHub) {
		t.Errorf("github sequences = %+v, want %+v", got, wantGitHub)
	}
	wantSlack := []Sequence{{Operations: []string{"x", "y", "z"}, Count: 1}}
	if got := got.Tools["slack"].Sequences; !reflect.DeepEqual(got, wantSlack) {
		t.Errorf("slack sequences = %+v, want %+v", got, wantSlack)
	}
}

func TestSummarizeIsDeterministic(t *testing.T) {
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	events := []Event{
		{Tool: "t", Operation: "a", Timestamp: base.Add(1 * time.Second), DurationMs: 1, Success: true},
		{Tool: "t", Operation: "b", Timestamp: base.Add(2 * time.Second), DurationMs: 2, Success: false, ErrorCode: "E"},
		{Tool: "t", Operation: "a", Timestamp: base.Add(3 * time.Second), DurationMs: 3, Success: true},
		{Tool: "t", Operation: "b", Timestamp: base.Add(4 * time.Second), DurationMs: 4, Success: true},
		{Tool: "u", Operation: "a", Timestamp: base.Add(1 * time.Second), DurationMs: 5, Success: true},
		{Tool: "u", Operation: "b", Timestamp: base.Add(2 * time.Second), DurationMs: 6, Success: true},
		{Tool: "u", Operation: "a", Timestamp: base.Add(3 * time.Second), DurationMs: 7, Success: true},
	}
	reversed := make([]Event, len(events))
	for i, e := range events {
		reversed[len(events)-1-i] = e
	}
	first, err := json.Marshal(Summarize(events))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(Summarize(events))
	if err != nil {
		t.Fatal(err)
	}
	third, err := json.Marshal(Summarize(reversed))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("summary is not stable across runs:\n%s\n%s", first, second)
	}
	if string(first) != string(third) {
		t.Errorf("summary depends on input order:\n%s\n%s", first, third)
	}
}
