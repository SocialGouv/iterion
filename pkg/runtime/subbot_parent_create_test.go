package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// recordingParentStore reports every run-creation call, so a test can pin
// WHICH create path the engine took rather than only the end state.
type recordingParentStore struct {
	store.RunStore
	plainCreates        []string
	childCreates        []string
	childParents        []string
	firstWrittenParents []string
}

// D1 calls the explicit birth port when it is present. Classify the birth's
// shape as before, and read it back before the engine can stamp a later SaveRun.
// This exposes no execution/accounting capabilities of the underlying store.
func (r *recordingParentStore) CreateRunWithValueFormat(ctx context.Context, spec store.RunCreateSpec) (*store.Run, error) {
	if spec.ParentRunID == "" {
		r.plainCreates = append(r.plainCreates, spec.ID)
	} else {
		r.childCreates = append(r.childCreates, spec.ID)
		r.childParents = append(r.childParents, spec.ParentRunID)
	}
	created, err := store.CreateRunWithValueFormat(ctx, r.RunStore, spec)
	if err != nil {
		return nil, err
	}
	first, err := r.LoadRun(ctx, spec.ID)
	if err != nil {
		return nil, err
	}
	r.firstWrittenParents = append(r.firstWrittenParents, first.ParentRunID)
	return created, nil
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
	storetest.SeedParentRun(t, base, "parent-run-1")
	rec := &recordingParentStore{RunStore: base}
	wf := &ir.Workflow{Name: "child_wf"}

	e := New(wf, rec, nil, WithParentRunID("parent-run-1"))
	_, run, err := e.runResolveDoc(context.Background(), "child-run-1", map[string]any{})
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
	if len(rec.firstWrittenParents) != 1 || rec.firstWrittenParents[0] != "parent-run-1" {
		t.Fatalf("first persisted birth parents = %v, want [parent-run-1] before any later stamp", rec.firstWrittenParents)
	}
	if run.ParentRunID != "parent-run-1" {
		t.Errorf("resolved run parent = %q, want parent-run-1", run.ParentRunID)
	}
}

// A top-level birth stays parentless, including through the explicit birth port.
func TestRunResolveDocKeepsATopLevelBirthParentless(t *testing.T) {
	base := tmpStore(t)
	rec := &recordingParentStore{RunStore: base}
	wf := &ir.Workflow{Name: "top_wf"}

	e := New(wf, rec, nil)
	if _, _, err := e.runResolveDoc(context.Background(), "top-run-1", map[string]any{}); err != nil {
		t.Fatalf("runResolveDoc: %v", err)
	}
	if len(rec.firstWrittenParents) != 1 || rec.firstWrittenParents[0] != "" {
		t.Fatalf("first persisted top-level parents = %v, want one parentless birth", rec.firstWrittenParents)
	}
	if len(rec.childCreates) != 0 {
		t.Errorf("a top-level run used the parented create: %v", rec.childCreates)
	}
	if len(rec.plainCreates) != 1 {
		t.Errorf("plain creates = %v, want exactly the one top-level run", rec.plainCreates)
	}
}
