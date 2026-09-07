package runview

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestListAllArtifacts seeds two nodes' artifacts (one with two versions)
// and verifies the aggregate returns the latest version per node with its
// labels and a derived title.
func TestListAllArtifacts(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := seed.CreateRun(ctx, "run1", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	write := func(node string, v int, labels []string, data map[string]any) {
		if err := seed.WriteArtifact(ctx, &store.Artifact{
			RunID: "run1", NodeID: node, Version: v, Labels: labels, Data: data,
		}); err != nil {
			t.Fatalf("write artifact %s v%d: %v", node, v, err)
		}
	}
	// planner: two versions; latest carries the plan label + a title.
	write("planner", 0, []string{"plan"}, map[string]any{"plan": "draft"})
	write("planner", 1, []string{"plan"}, map[string]any{"plan": "final", "title": "Migration plan"})
	// reviewer: one version, verdict label, no title.
	write("reviewer", 0, []string{"verdict"}, map[string]any{"approved": true})

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	got, err := svc.ListAllArtifacts("run1")
	if err != nil {
		t.Fatalf("ListAllArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2: %+v", len(got), got)
	}
	// Sorted by node id: planner, reviewer.
	if got[0].NodeID != "planner" || got[0].Version != 1 {
		t.Errorf("planner: got node=%s v=%d, want planner v1", got[0].NodeID, got[0].Version)
	}
	if got[0].Title != "Migration plan" {
		t.Errorf("planner title = %q, want %q", got[0].Title, "Migration plan")
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "plan" {
		t.Errorf("planner labels = %v, want [plan]", got[0].Labels)
	}
	if got[1].NodeID != "reviewer" || len(got[1].Labels) != 1 || got[1].Labels[0] != "verdict" {
		t.Errorf("reviewer: got %+v, want verdict label", got[1])
	}

	// Unknown run → empty, no error.
	empty, err := svc.ListAllArtifacts("nope")
	if err != nil {
		t.Fatalf("ListAllArtifacts(nope): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown run returned %d artifacts, want 0", len(empty))
	}
}

// TestListAllArtifacts_FromIndexWhenNoDirectory is the cloud shape: the
// server pod has no runs/<id>/artifacts directory (the runner that wrote
// the artifacts has it), but the run document carries artifact_index and
// the store can load every version. The listing must come from there
// instead of reading as an empty run.
func TestListAllArtifacts_FromIndexWhenNoDirectory(t *testing.T) {
	storeRoot := t.TempDir()
	logger := iterlog.Nop()
	seed, err := store.New(storeRoot, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := seed.CreateRun(ctx, "run2", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, v := range []int{0, 1} {
		if err := seed.WriteArtifact(ctx, &store.Artifact{RunID: "run2", NodeID: "report", Version: v, Labels: []string{"report"}, Data: map[string]any{"title": "Report v" + string(rune('0'+v))}}); err != nil {
			t.Fatalf("write artifact v%d: %v", v, err)
		}
	}
	if err := seed.WriteArtifact(ctx, &store.Artifact{RunID: "run2", NodeID: "verdict", Version: 0, Data: map[string]any{"ok": true}}); err != nil {
		t.Fatalf("write verdict: %v", err)
	}

	// A service whose storeDir holds NO artifact directory for the run,
	// backed by the store that does know the run.
	svc, err := NewService(t.TempDir(), WithLogger(logger), WithStore(seed))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	got, err := svc.ListAllArtifacts("run2")
	if err != nil {
		t.Fatalf("ListAllArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2 (from artifact_index): %+v", len(got), got)
	}
	if got[0].NodeID != "report" || got[0].Version != 1 || got[0].Title != "Report v1" || len(got[0].Labels) != 1 {
		t.Errorf("report: %+v, want the latest version with its label and title", got[0])
	}
	if got[1].NodeID != "verdict" || got[1].Version != 0 {
		t.Errorf("verdict: %+v", got[1])
	}
	empty, err := svc.ListAllArtifacts("unknown")
	if err != nil || len(empty) != 0 {
		t.Errorf("unknown run: got %v, %v; want empty, nil", empty, err)
	}
}
