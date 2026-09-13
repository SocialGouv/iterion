package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResolveAssistantCardFollowsLastRun(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(ctx, "run-cancelled", "copilot", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = board.Close() })
	issue, err := board.Create(native.Issue{ID: "native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf", Title: "Assistant diagnosis", State: "awaiting_input"})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.SetLastRun(issue.ID, run.ID, "/must/not/leak"); err != nil {
		t.Fatal(err)
	}

	got, err := resolveAssistantReference(ctx, "card/native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf", rs, board)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Resolved || got.Task == nil || got.Task.LastRunID != run.ID {
		t.Fatalf("task resolution = %#v", got)
	}
	if got.Run == nil || got.Run.Status != store.RunStatusCancelled {
		t.Fatalf("run resolution = %#v", got.Run)
	}
	if !got.Run.Resumable {
		t.Fatalf("cancelled run must be stamped resumable: %#v", got.Run)
	}
}

func TestLoadAssistantRunSeparatesRewindFromResumeAndSourceRepair(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(ctx, "run-failed", "workflow", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailed
	run.Error = "workflow reached fail node"
	run.Checkpoint = &store.Checkpoint{
		NodeID:  "fail",
		Outputs: map[string]map[string]any{"prepare": {"status": "ok"}},
	}
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	got, err := loadAssistantRun(ctx, run.ID, rs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Resumable {
		t.Fatalf("terminal fail must not be stamped directly resumable: %#v", got)
	}
	if !got.Rewindable {
		t.Fatalf("terminal fail with checkpoint must be stamped rewindable: %#v", got)
	}
	if got.Repair == nil || got.Repair.Repairable {
		t.Fatalf("source repair should remain unavailable without provenance: %#v", got.Repair)
	}
}

func TestLoadAssistantRunDoesNotAdvertiseRewindWithoutCheckpoint(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(ctx, "run-bootstrap-failed", "workflow", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailed
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	got, err := loadAssistantRun(ctx, run.ID, rs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rewindable {
		t.Fatalf("failed run without checkpoint must not be stamped rewindable: %#v", got)
	}
}

func TestResolveAssistantReferenceRejectsTraversalAndUnknownKinds(t *testing.T) {
	for _, ref := range []string{"run/../../other", "card/native:d5fc/extra", "repo/org/name"} {
		got, err := resolveAssistantReference(context.Background(), ref, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if got.Resolved || got.Reason == "" {
			t.Fatalf("%s unexpectedly resolved: %#v", ref, got)
		}
	}
}

func TestAssistantContextReferencesInfersNativeTaskFromOperatorMessage(t *testing.T) {
	got := assistantContextReferences(
		[]string{"view/pipelines", "card/native:already-present"},
		"Peux-tu me dire pourquoi le run #native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf a failed ?",
	)
	want := []string{
		"view/pipelines",
		"card/native:already-present",
		"card/native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf",
	}
	if len(got) != len(want) {
		t.Fatalf("references = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("references[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAssistantContextReferencesDeduplicatesNativeTask(t *testing.T) {
	got := assistantContextReferences(
		[]string{"card/native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf"},
		"native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf puis #native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf",
	)
	if len(got) != 1 || got[0] != "card/native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf" {
		t.Fatalf("references = %#v", got)
	}
}
