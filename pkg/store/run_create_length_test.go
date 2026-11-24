package store

import (
	"errors"
	"strings"
	"testing"
)

// A run this store cannot stream must never be created: the ID bound the
// stream open enforces binds creation too (a 256-byte multi-byte ID would
// otherwise be created here and refused by every event or log stream).
func TestCreateRunRefusesAnIDTooLongForItsStreams(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateRun(t.Context(), strings.Repeat("é", 128), "wf", nil)
	if !errors.Is(err, ErrInvalidRunDataIdentity) {
		t.Fatalf("CreateRun = %v, want ErrInvalidRunDataIdentity", err)
	}
	if _, err := s.CreateRun(t.Context(), strings.Repeat("é", 127), "wf", nil); err != nil {
		t.Fatalf("a 254-byte ID is legal: %v", err)
	}
}

// The birth path opens an ID-named lock file before publication: the same ID
// bound applies before that open, so an over-long ID is refused typed, not by
// the OS in its own words (H-10 round 5).
func TestBeginRunBirthRefusesAnIDTooLongForItsStreams(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("x", 300)
	ctx, _ := WithRunValueFormat(t.Context(), RunValueFormat{RunID: id, Format: LegacyValueFormat})
	b, err := s.BeginRunBirth(ctx, id)
	if err == nil {
		t.Fatal("BeginRunBirth accepted a 300-byte ID")
	}
	if !errors.Is(err, ErrInvalidRunDataIdentity) {
		t.Fatalf("BeginRunBirth = %v, want ErrInvalidRunDataIdentity", err)
	}
	_ = b
}
