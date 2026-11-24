package store

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

// A run.json that does not decode is reported as ErrRunMetadataUndecodable —
// syntax, type, truncation — while an empty document (a bare io.EOF) keeps
// its own error: a reader can meet it mid-write.
func TestLoadRunReportsUndecodableMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		body       string
		undecoded  bool
		bareEOFErr bool
	}{
		{"syntax", `{"id": "r", not json`, true, false},
		{"truncated", `{"id": "r", "workflow_name": "wf"`, true, false},
		{"cut-mid-token", `{"id": "r", "workflow_na`, true, false},
		{"type", `{"id": "r", "workflow_name": 7}`, true, false},
		{"empty", ``, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.runJSONPath("r"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = s.LoadRun(t.Context(), "r")
			if err == nil {
				t.Fatal("a broken run.json loaded")
			}
			if got := errors.Is(err, ErrRunMetadataUndecodable); got != tc.undecoded {
				t.Fatalf("undecodable = %v, want %v: %v", got, tc.undecoded, err)
			}
			if tc.bareEOFErr && !errors.Is(err, io.EOF) {
				t.Fatalf("empty document: %v, want its io.EOF", err)
			}
		})
	}
}

// Context errors are never reported as undecodable metadata.
func TestRunMetadataUndecodableKeepsContextErrors(t *testing.T) {
	t.Parallel()
	for _, err := range []error{nil, errors.Join(errors.New("x"), context.Canceled), context.DeadlineExceeded} {
		if got := RunMetadataUndecodable("r", err); errors.Is(got, ErrRunMetadataUndecodable) {
			t.Fatalf("%v reported undecodable", err)
		}
	}
	if got := RunMetadataUndecodable("r", errors.New("bson: bad document")); !errors.Is(got, ErrRunMetadataUndecodable) {
		t.Fatalf("decode failure not reported: %v", got)
	}
}
