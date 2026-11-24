package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The same contract on FS and Mongo: a birth survives execution transitions,
// while delete/recreate is a new identity even if the public ID is reused.
func testBirthIdentity(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	r, err := s.CreateRun(ctx, "birth", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := r.BirthID
	if first == "" {
		t.Fatal("create returned no birth identity")
	}
	load := func() *store.Run {
		t.Helper()
		r, e := s.LoadRun(ctx, "birth")
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	check := func() *store.Run {
		t.Helper()
		r := load()
		if r.BirthID != first {
			t.Fatalf("birth changed: %q -> %q", first, r.BirthID)
		}
		return r
	}
	r.BirthID = "forged"
	if err = s.SaveRun(ctx, r); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("forged birth accepted: %v", err)
	}
	r = check()
	r.BirthID = ""
	r.Name = "metadata"
	if err = s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r.BirthID != first {
		t.Fatal("save response lost original birth")
	}
	r = check()
	initialEpoch := r.ExecutionEpoch
	if err = s.PauseRun(ctx, r.ID, &store.Checkpoint{NodeID: "pause"}); err != nil {
		t.Fatal(err)
	}
	r = check()
	if r.ExecutionEpoch == initialEpoch {
		t.Fatal("fixture did not rotate execution epoch")
	}
	for _, status := range []store.RunStatus{store.RunStatusQueued, store.RunStatusRunning, store.RunStatusFinished} {
		if err = s.UpdateRunStatus(ctx, r.ID, status, ""); err != nil {
			t.Fatal(err)
		}
		check()
	}
	// SaveRun's genuine insertion route also owns identity, never its caller.
	direct := &store.Run{ID: "direct", WorkflowName: "wf", Status: store.RunStatusRunning, BirthID: "caller-supplied"}
	if err = s.SaveRun(ctx, direct); err != nil {
		t.Fatal(err)
	}
	if direct.BirthID == "" || direct.BirthID == "caller-supplied" {
		t.Fatal("upsert retained caller birth")
	}
	loaded, err := s.LoadRun(ctx, direct.ID)
	if err != nil || loaded.BirthID != direct.BirthID {
		t.Fatalf("upsert response mismatch: %v", err)
	}
	if err = s.DeleteRun(ctx, "birth"); err != nil {
		t.Fatal(err)
	}
	if n, e := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); e != nil || n != 1 {
		t.Fatalf("prune=%d %v", n, e)
	}
	replacement, err := s.CreateRun(ctx, "birth", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.BirthID == "" || replacement.BirthID == first {
		t.Fatal("recreated run reused birth")
	}
	// Even a caller with the replacement's current CAS version cannot use the
	// old incarnation to overwrite it.
	replacement.BirthID = first
	if err = s.SaveRun(ctx, replacement); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("old incarnation accepted: %v", err)
	}
}
