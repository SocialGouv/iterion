package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestRunFallbackJSONAcceptsObjectAndArray(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want RunFallback
	}{
		{
			name: "legacy object",
			raw:  `{"fallback":{"backend":"codex","model":"gpt-5.5"}}`,
			want: RunFallback{{Backend: "codex", Model: "gpt-5.5"}},
		},
		{
			name: "ordered array",
			raw:  `{"fallback":[{"backend":"codex","model":"gpt-5.5"},{"backend":"claw","model":"openai/gpt-5.5"}]}`,
			want: RunFallback{
				{Backend: "codex", Model: "gpt-5.5"},
				{Backend: "claw", Model: "openai/gpt-5.5"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var run Run
			if err := json.Unmarshal([]byte(tc.raw), &run); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(run.Fallback, tc.want) {
				t.Fatalf("fallback = %+v, want %+v", run.Fallback, tc.want)
			}
			blob, err := json.Marshal(run)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Contains(blob, []byte(`"fallback":[`)) {
				t.Fatalf("canonical fallback is not an array: %s", blob)
			}
		})
	}
}

// sliceEq compares two string slices under the watched-issue helpers'
// contract: a nil want means "must be exactly nil" (so the persisted
// field stays omitted), distinct from a non-nil empty slice.
func sliceEq(got, want []string) bool {
	if want == nil {
		return got == nil
	}
	return reflect.DeepEqual(got, want)
}

// IsTerminal classifies whether polling consumers can stop refreshing a
// run. Exactly the four states below are terminal; everything else is
// live. A regression that mis-buckets any state would either spin the
// poller forever or drop a still-running run from the dashboard.
func TestRunStatusIsTerminal(t *testing.T) {
	tests := []struct {
		status RunStatus
		want   bool
	}{
		{RunStatusFinished, true},
		{RunStatusFailed, true},
		{RunStatusFailedResumable, true},
		{RunStatusCancelled, true},
		{RunStatusRunning, false},
		{RunStatusPausedWaitingHuman, false},
		{RunStatusPausedOperator, false},
		{RunStatusQueued, false},
		{RunStatus("definitely-not-a-status"), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := tt.status.IsTerminal(); got != tt.want {
				t.Errorf("RunStatus(%q).IsTerminal() = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestDedupeNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"drops empties", []string{"", "a", ""}, []string{"a"}},
		{"dedups preserving first-seen order", []string{"b", "a", "b"}, []string{"b", "a"}},
		{"interleaved empties and dups", []string{"", "x", "y", "x", "", "z"}, []string{"x", "y", "z"}},
		{"all empty returns nil", []string{"", "", ""}, nil},
		{"nil returns nil", nil, nil},
		{"empty slice returns nil", []string{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dedupeNonEmpty(tt.in); !sliceEq(got, tt.want) {
				t.Errorf("dedupeNonEmpty(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestMergeWatchedIssues(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		add      []string
		want     []string
	}{
		{"existing leads, union deduped", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{"add into empty", nil, []string{"a", "a", "b"}, []string{"a", "b"}},
		{"add empty keeps existing", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"both empty returns nil", nil, nil, nil},
		{"empties dropped from union", []string{"a", ""}, []string{"", "b"}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeWatchedIssues(tt.existing, tt.add); !sliceEq(got, tt.want) {
				t.Errorf("mergeWatchedIssues(%v, %v) = %v, want %v", tt.existing, tt.add, got, tt.want)
			}
		})
	}
}

func TestRemoveWatchedIssues(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		drop     []string
		want     []string
	}{
		{"drops listed, preserves remaining order", []string{"a", "b", "c"}, []string{"a"}, []string{"b", "c"}},
		{"drop-id-absent leaves set unchanged", []string{"a", "b"}, []string{"z"}, []string{"a", "b"}},
		{"removing all returns nil", []string{"a", "b"}, []string{"a", "b"}, nil},
		{"empty existing returns nil", nil, []string{"a"}, nil},
		{"empty drop leaves set", []string{"a", "b"}, nil, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := removeWatchedIssues(tt.existing, tt.drop); !sliceEq(got, tt.want) {
				t.Errorf("removeWatchedIssues(%v, %v) = %v, want %v", tt.existing, tt.drop, got, tt.want)
			}
		})
	}
}
