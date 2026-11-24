package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestRunMetadataPublicFilesystemBinding(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: 2}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.runJSONPath("r"))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "r", Format: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunnerVersion(bound, "r", "wrong"); !errors.Is(err, ErrValueFormatChanged) {
		t.Fatalf("public binding err=%v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.SetRunnerVersion(cancelled, "r", "wrong"); !errors.Is(err, context.Canceled) {
		t.Fatalf("public cancellation err=%v", err)
	}
	after, err := os.ReadFile(s.runJSONPath("r"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed run", err)
	}
}
