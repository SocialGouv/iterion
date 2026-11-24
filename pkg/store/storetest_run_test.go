package store_test

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// TestConformance_FilesystemShared runs the EXPORTED cross-backend
// conformance suite (pkg/store/storetest) against the filesystem backend.
//
// pkg/store/conformance_test.go already runs an internal copy of the core
// assertions, but the shared suite is the one the Mongo backend plugs into
// — and it additionally covers the optional seams (DeleteRun, RunLogStore,
// TurnStore, …). Running the SAME harness against the filesystem store
// here means every seam the cloud twin must satisfy is proven identical on
// both backends, in-tree, without a live Mongo.
func TestConformance_FilesystemShared(t *testing.T) {
	storetest.RunWithOpts(t, func(t *testing.T) store.RunStore {
		t.Helper()
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		return s
	}, storetest.Opts{InitialStatus: store.RunStatusRunning})
}

func TestToolApprovalSpendSurvivesFilesystemReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	open := func() *store.FilesystemRunStore {
		t.Helper()
		s, err := store.New(root)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	const runID = "approval-reopen"
	s := open()
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "gate", InteractionID: "choice"}
	if err := s.PauseRun(ctx, runID, cp); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	claim := store.ToolApprovalClaim{InteractionID: "choice", Token: "first"}
	if won, err := s.ClaimToolApprovalResume(ctx, runID, claim, r.Status, r.CASVersion); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if won, err := s.SpendToolApproval(ctx, runID, claim); err != nil || !won {
		t.Fatalf("spend: won=%v err=%v", won, err)
	}
	s = open()
	if err := s.PauseRun(ctx, runID, cp); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	claim.Token = "second"
	if won, err := s.ClaimToolApprovalResume(ctx, runID, claim, r.Status, r.CASVersion); err != nil || won {
		t.Fatalf("spent approval after reopen: won=%v err=%v", won, err)
	}
}

func TestConformance_FilesystemCodemodeScratch(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storetest.CodemodeScratch(t, s, store.Open(s.Root()))
}
