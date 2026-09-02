package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The checkpoint survives every status transition (ADR-095), so a
// FINISHED run keeps whatever interaction pointer it held. Inspect must
// not report "Paused at" + a live "Interaction" id on a terminated run.
func TestInspectHidesStalePauseOnFinishedRun(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-stale", "wf", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "gate", InteractionID: "I1"}
	if err := s.PauseRun(ctx, "r-stale", cp); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, "r-stale", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	p := &Printer{Format: OutputHuman, W: &buf}
	if err := RunInspect(InspectOptions{RunID: "r-stale", StoreDir: dir}, p); err != nil {
		t.Fatalf("RunInspect: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "Paused at") || strings.Contains(out, "I1") {
		t.Errorf("inspect of a FINISHED run advertises a pause:\n%s", out)
	}
}

// The middle of the matrix: a failed_resumable run keeps its anchor
// (where the post-mortem starts) but must not advertise a pause — this
// is the line that distinguishes the NodeID gate from an IsPaused one.
func TestInspectShowsAnchorWithoutPauseOnFailedResumable(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-parked", "wf", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "implement", InteractionID: "I1"}
	if err := s.PauseRun(ctx, "r-parked", cp); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, "r-parked", store.RunStatusFailedResumable, "parked"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	p := &Printer{Format: OutputHuman, W: &buf}
	if err := RunInspect(InspectOptions{RunID: "r-parked", StoreDir: dir}, p); err != nil {
		t.Fatalf("RunInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "implement") {
		t.Errorf("inspect of a parked run lost its checkpoint anchor:\n%s", out)
	}
	if strings.Contains(out, "Paused at") || strings.Contains(out, "I1") {
		t.Errorf("inspect of a parked run advertises a pause:\n%s", out)
	}
}

// On a genuinely paused run the block stays: the pointer is live.
func TestInspectShowsPausePointerOnPausedRun(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-paused", "wf", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "gate", InteractionID: "I1"}
	if err := s.PauseRun(ctx, "r-paused", cp); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	p := &Printer{Format: OutputHuman, W: &buf}
	if err := RunInspect(InspectOptions{RunID: "r-paused", StoreDir: dir}, p); err != nil {
		t.Fatalf("RunInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Paused at") || !strings.Contains(out, "I1") {
		t.Errorf("inspect of a PAUSED run lost its pause pointer:\n%s", out)
	}
}
