package relay

import "testing"

func TestCompatibleRuntimeAPIVersion(t *testing.T) {
	tests := []struct {
		required string
		provided string
		want     bool
	}{
		{required: "v1", provided: "v1", want: true},
		{required: "v1.2", provided: "v1", want: true},
		{required: "v1", provided: "v1.9", want: true},
		{required: "", provided: "v1", want: true},
		{required: "v2", provided: "v1", want: false},
		{required: "v1", provided: "v2", want: false},
		{required: "garbage", provided: "v1", want: false},
	}
	for _, test := range tests {
		if got := CompatibleRuntimeAPIVersion(test.required, test.provided); got != test.want {
			t.Errorf("CompatibleRuntimeAPIVersion(%q, %q) = %v, want %v",
				test.required, test.provided, got, test.want)
		}
	}
}
