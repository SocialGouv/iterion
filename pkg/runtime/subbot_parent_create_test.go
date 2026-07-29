package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// recordingParentStore reports every run-creation call, so a test can pin
// WHICH create path the engine took rather than only the end state.
type recordingParentStore struct {
	store.RunStore
	plainCreates []string
	childCreates []string
	childParents []string
}

func (r *recordingParentStore) CreateRun(ctx context.Context, id, wf string, inputs map[string]any) (*store.Run, error) {
	r.plainCreates = append(r.plainCreates, id)
	return r.RunStore.CreateRun(ctx, id, wf, inputs)
}

func (r *recordingParentStore) CreateChildRun(ctx context.Context, id, wf, parentRunID string, inputs map[string]any) (*store.Run, error) {
	r.childCreates = append(r.childCreates, id)
	r.childParents = append(r.childParents, parentRunID)
	pc := store.AsParentedRunCreator(r.RunStore)
	if pc == nil {
		t := &store.Run{ID: id, WorkflowName: wf, ParentRunID: parentRunID}
		return t, nil
	}
	return pc.CreateChildRun(ctx, id, wf, parentRunID, inputs)
}

// A child's run row must carry its parent from its FIRST write. Creating it
// with CreateRun and stamping ParentRunID in a follow-up SaveRun left a window
// where the row existed, was `running`, and had no parent — and a row with no
// parent is indistinguishable from a top-level run, which is exactly what the
// orphan reconciler judges. Every subbot child is created through this path.
func TestRunResolveDocCreatesAChildWithItsParentAtomically(t *testing.T) {
	base := tmpStore(t)
	rec := &recordingParentStore{RunStore: base}
	wf := &ir.Workflow{Name: "child_wf"}

	e := New(wf, rec, nil, WithParentRunID("parent-run-1"))
	run, err := e.runResolveDoc(context.Background(), "child-run-1", map[string]any{})
	if err != nil {
		t.Fatalf("runResolveDoc: %v", err)
	}

	if len(rec.childCreates) != 1 {
		t.Fatalf("child created via the plain path (%v) instead of an atomic parented create", rec.plainCreates)
	}
	if rec.childParents[0] != "parent-run-1" {
		t.Errorf("created with parent %q, want parent-run-1", rec.childParents[0])
	}
	if len(rec.plainCreates) != 0 {
		t.Errorf("unexpected parentless create: %v", rec.plainCreates)
	}
	if run.ParentRunID != "parent-run-1" {
		t.Errorf("resolved run parent = %q, want parent-run-1", run.ParentRunID)
	}
}

// A top-level run has no parent and must keep using the plain create.
func TestRunResolveDocKeepsThePlainCreateForATopLevelRun(t *testing.T) {
	base := tmpStore(t)
	rec := &recordingParentStore{RunStore: base}
	wf := &ir.Workflow{Name: "top_wf"}

	e := New(wf, rec, nil)
	if _, err := e.runResolveDoc(context.Background(), "top-run-1", map[string]any{}); err != nil {
		t.Fatalf("runResolveDoc: %v", err)
	}
	if len(rec.childCreates) != 0 {
		t.Errorf("a top-level run used the parented create: %v", rec.childCreates)
	}
	if len(rec.plainCreates) != 1 {
		t.Errorf("plain creates = %v, want exactly the one top-level run", rec.plainCreates)
	}
}
