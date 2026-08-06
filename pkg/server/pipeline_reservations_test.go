package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedReservedRun creates one run in the store behind the runview service.
func seedReservedRun(t *testing.T, st store.RunStore, id string, mutate func(*store.Run)) {
	t.Helper()
	if _, err := st.CreateRun(context.Background(), id, "review", nil); err != nil {
		t.Fatalf("CreateRun(%s): %v", id, err)
	}
	run, err := st.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", id, err)
	}
	mutate(run)
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun(%s): %v", id, err)
	}
}

// The reserved set is composed as holdsSlot && !treeAwaitingHuman — and
// the tree walk sees BOTH the fork and its dead parent (the fork
// inherits Source.IssueID and carries ParentRunID). The pre-existing
// release assumes the live tree holds a real queue slot, which a fork
// never does (Resume never enters pipelineQueue): a fork parked on a
// human gate, or forked from a parent stuck paused_waiting_human, must
// KEEP the reservation or admission over-admits (R5bc762).
func TestPipelineReservedSetKeepsNonTerminalForkDespiteHumanGate(t *testing.T) {
	dir := t.TempDir()
	board, err := native.NewStore(filepath.Join(dir, "board"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runview.NewService(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := board.Create(native.Issue{Title: "epic", Bot: "review", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.SetLastRun(issue.ID, "run-parent", ""); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	seedReservedRun(t, st, "run-parent", func(run *store.Run) {
		run.Status = store.RunStatusFailed
		run.CreatedAt = older
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
	})
	// The fork is parked ON A HUMAN GATE: alive, non-terminal, and the
	// tree walk sees it.
	seedReservedRun(t, st, "run-fork", func(run *store.Run) {
		run.Status = store.RunStatusPausedWaitingHuman
		// Created after the SetLastRun above (default: now) — the
		// production ordering: a fork is always minted after the
		// parent's attempt was registered.
		run.ForkedFrom = "run-parent"
		run.ParentRunID = "run-parent"
		run.Source = &store.RunSource{IssueID: issue.ID}
	})

	srv := &Server{logger: iterlog.New(iterlog.LevelError, nil)}
	set := srv.pipelineReservedSetMemo(&pipelineReservedMemo{}, board, runs)
	if _, reserved := set[issue.ID]; !reserved {
		t.Error("a non-terminal fork parked on a human gate must keep the ticket's reserved slot (it holds no queue slot)")
	}
}

// Same shape with a parent stuck paused_waiting_human forever: the tree
// walk triggers on the PARENT this time. The fork runs — it must keep
// the slot for the same reason.
func TestPipelineReservedSetKeepsRunningForkDespitePausedParent(t *testing.T) {
	dir := t.TempDir()
	board, err := native.NewStore(filepath.Join(dir, "board"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runview.NewService(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := board.Create(native.Issue{Title: "epic", Bot: "review", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.SetLastRun(issue.ID, "run-parent", ""); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	seedReservedRun(t, st, "run-parent", func(run *store.Run) {
		run.Status = store.RunStatusFailed
		run.CreatedAt = older
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
	})
	// A SECOND run of the ticket tree, forever paused on a human gate.
	seedReservedRun(t, st, "run-paused-sibling", func(run *store.Run) {
		run.Status = store.RunStatusPausedWaitingHuman
		run.CreatedAt = older.Add(10 * time.Minute)
		run.ParentRunID = "run-parent"
		run.Source = &store.RunSource{IssueID: issue.ID}
	})
	seedReservedRun(t, st, "run-fork", func(run *store.Run) {
		run.Status = store.RunStatusRunning
		// Created after the SetLastRun above (default: now), as in
		// production: the fork post-dates the parent's attempt.
		run.ForkedFrom = "run-parent"
		run.ParentRunID = "run-parent"
		run.Source = &store.RunSource{IssueID: issue.ID}
	})

	srv := &Server{logger: iterlog.New(iterlog.LevelError, nil)}
	set := srv.pipelineReservedSetMemo(&pipelineReservedMemo{}, board, runs)
	if _, reserved := set[issue.ID]; !reserved {
		t.Error("a running fork must keep the reserved slot even when a sibling of its tree awaits a human")
	}
}

// The pre-existing behavior is preserved for non-fork trees: a failed
// ticket whose tree awaits a human does NOT reserve (the live run holds
// a real queue slot).
func TestPipelineReservedSetReleasesNonForkTreeAwaitingHuman(t *testing.T) {
	dir := t.TempDir()
	board, err := native.NewStore(filepath.Join(dir, "board"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runview.NewService(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := board.Create(native.Issue{Title: "epic", Bot: "review", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.SetLastRun(issue.ID, "run-parent", ""); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	seedReservedRun(t, st, "run-parent", func(run *store.Run) {
		run.Status = store.RunStatusFailed
		run.CreatedAt = older
		run.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issue.ID}
	})
	seedReservedRun(t, st, "run-child", func(run *store.Run) {
		run.Status = store.RunStatusPausedWaitingHuman
		run.CreatedAt = older.Add(10 * time.Minute)
		run.ParentRunID = "run-parent"
	})

	srv := &Server{logger: iterlog.New(iterlog.LevelError, nil)}
	set := srv.pipelineReservedSetMemo(&pipelineReservedMemo{}, board, runs)
	if _, reserved := set[issue.ID]; reserved {
		t.Error("a non-fork tree parked on a human gate must NOT reserve (pre-existing behavior)")
	}
}
