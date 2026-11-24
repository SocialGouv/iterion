package storetest

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func testToolApprovalSpend(t *testing.T, s store.RunStore) {
	spender, ok := s.(store.ToolApprovalSpender)
	if !ok {
		t.Fatal("store does not support tool approval consumption")
	}
	ctx := testCtx()
	const runID = "approval-run"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	load := func() *store.Run {
		t.Helper()
		r, err := s.LoadRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	move := func(status store.RunStatus) {
		t.Helper()
		if err := s.UpdateRunStatus(ctx, runID, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	spend := func(claim store.ToolApprovalClaim, want bool) {
		t.Helper()
		won, err := spender.SpendToolApproval(ctx, runID, claim)
		if err != nil || won != want {
			t.Fatalf("spend %q: won=%v err=%v, want won=%v", claim.InteractionID, won, err, want)
		}
	}
	pause := func(id string) *store.Run {
		t.Helper()
		if err := s.PauseRun(ctx, runID, &store.Checkpoint{NodeID: "gate", InteractionID: id}); err != nil {
			t.Fatal(err)
		}
		return load()
	}
	claimResume := func(claim store.ToolApprovalClaim, r *store.Run, want bool) {
		t.Helper()
		won, err := spender.ClaimToolApprovalResume(ctx, runID, claim, r.Status, r.CASVersion)
		if err != nil || won != want {
			t.Fatalf("claim %q: won=%v err=%v, want won=%v", claim.InteractionID, won, err, want)
		}
	}
	old := store.ToolApprovalClaim{InteractionID: "choice", Token: "first"}
	claim := store.ToolApprovalClaim{InteractionID: "choice", Token: "second"}
	paused := pause("choice")
	claimResume(store.ToolApprovalClaim{InteractionID: "other", Token: "first"}, paused, false)
	updated := load()
	updated.Error = "metadata changed while paused"
	if err := s.SaveRun(ctx, updated); err != nil {
		t.Fatal(err)
	}
	claimResume(old, paused, false)
	claimResume(old, load(), true)
	spend(claim, false)
	paused = pause("choice")
	spend(old, false)
	move(store.RunStatusRunning)
	spend(old, false)
	claimResume(claim, paused, false)
	claimResume(claim, pause("choice"), true)
	// Routine metadata writes neither mint nor invalidate a claim.
	active := load()
	active.ToolApprovalClaim = &old
	if err := s.SaveRun(ctx, active); err != nil {
		t.Fatal(err)
	}
	spend(old, false)
	grants := map[string][]string{"gate": {"choose_label"}}
	if won, err := spender.GrantOwnedToolPermission(ctx, runID, old, grants); err != nil || won {
		t.Fatalf("superseded grant: won=%v err=%v", won, err)
	}
	if len(load().PermissionGrants) != 0 {
		t.Fatal("superseded claim wrote permission grants")
	}
	if won, err := spender.GrantOwnedToolPermission(ctx, runID, claim, grants); err != nil || !won {
		t.Fatalf("owned grant: won=%v err=%v", won, err)
	}
	if got := load().PermissionGrants["gate"]; len(got) != 1 || got[0] != "choose_label" {
		t.Fatalf("owned grant not persisted: %v", got)
	}
	type result struct {
		won bool
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			won, err := spender.SpendToolApproval(ctx, runID, claim)
			results <- result{won, err}
		}()
	}
	wins := 0
	for range 2 {
		res := <-results
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.won {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent spends won %d times, want 1", wins)
	}
	r := load()
	r.SpentToolApprovals = []string{"fabricated"}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r = load()
	if len(r.SpentToolApprovals) != 1 || r.SpentToolApprovals[0] != "choice" {
		t.Fatalf("metadata save changed spent approvals: %v", r.SpentToolApprovals)
	}
	spend(claim, false)
	if r.UnresolvedToolApproval != claim.InteractionID {
		t.Fatal("metadata save lost unresolved dispatch")
	}
	if code, err := spender.FailOwnedToolApproval(ctx, runID, old, &store.Checkpoint{NodeID: "stale"}, "stale failure", store.FailureExecutionFailed); err != nil || code != "" {
		t.Fatalf("superseded failure: code=%q err=%v", code, err)
	}
	if got := load(); got.Status != store.RunStatusRunning || got.UnresolvedToolApproval != claim.InteractionID {
		t.Fatal("superseded failure changed the active run")
	}
	continuation := &store.Checkpoint{NodeID: "successor"}
	if won, err := spender.CommitToolApprovalContinuation(ctx, runID, old, continuation); err != nil || won {
		t.Fatalf("superseded continuation: won=%v err=%v", won, err)
	}
	if won, err := spender.CommitToolApprovalContinuation(ctx, runID, claim, continuation); err != nil || !won {
		t.Fatalf("owned continuation: won=%v err=%v", won, err)
	}
	r = load()
	if r.UnresolvedToolApproval != "" || r.ToolApprovalClaim != nil || r.Checkpoint == nil || r.Checkpoint.NodeID != "successor" {
		t.Fatal("continuation did not atomically replace unresolved dispatch")
	}
	claimResume(store.ToolApprovalClaim{InteractionID: "choice", Token: "third"}, pause("choice"), false)
	next := store.ToolApprovalClaim{InteractionID: "another-choice", Token: "next"}
	paused = pause("another-choice")
	flipper := store.AsRevisionQueuedFlipper(s)
	if flipper == nil {
		t.Fatal("store does not support revision-bound queue transitions")
	}
	latest := pause("another-choice")
	if _, won, err := flipper.FlipRevisionToQueued(ctx, runID, paused.Status, time.Now(), paused.CASVersion); err != nil || won {
		t.Fatalf("stale revision queue flip: won=%v err=%v, want unchanged pause", won, err)
	}
	if _, won, err := flipper.FlipRevisionToQueued(ctx, runID, latest.Status, time.Now(), latest.CASVersion); err != nil || !won {
		t.Fatalf("current revision queue flip: won=%v err=%v, want queued run", won, err)
	}
	claimResume(next, latest, false)
	claimResume(next, load(), true)
	spend(next, true)
	pending := &store.Checkpoint{NodeID: "gate", InteractionID: "next-question"}
	if won, err := spender.CommitToolApprovalContinuation(ctx, runID, next, pending); err != nil || !won {
		t.Fatalf("owned pause: won=%v err=%v", won, err)
	}
	r = load()
	if r.Status != store.RunStatusPausedWaitingHuman || r.UnresolvedToolApproval != "" || r.ToolApprovalClaim != nil || r.Checkpoint == nil || r.Checkpoint.InteractionID != pending.InteractionID {
		t.Fatal("new pause did not atomically replace unresolved dispatch")
	}

	final := store.ToolApprovalClaim{InteractionID: pending.InteractionID, Token: "final"}
	claimResume(final, r, true)
	spend(final, true)
	if code, err := spender.FailOwnedToolApproval(ctx, runID, final, &store.Checkpoint{NodeID: "gate"}, "interrupted", store.FailureExecutionFailed); err != nil || code != store.FailureAmbiguousEffect {
		t.Fatalf("owned failure: code=%q err=%v", code, err)
	}
	r = load()
	if r.FailureCode != store.FailureAmbiguousEffect || r.UnresolvedToolApproval != final.InteractionID || r.ToolApprovalClaim != nil {
		t.Fatal("owned failure did not preserve unresolved dispatch")
	}
	// An exact pause is a cycle of its own: pausing with captured input must
	// store no unresolved marker (a Mongo "" would poison every later claim
	// and flip), and the acknowledged queue flip may pass a run an earlier
	// ambiguous dispatch left unresolved — the documented --force recovery.
	exact := &store.Checkpoint{NodeID: "gate", InteractionID: "exact-pause", BackendPermissionInput: []byte(`{"label":"cyan"}`)}
	if err := s.PauseRun(ctx, runID, exact); err != nil {
		t.Fatal(err)
	}
	exactRun := load()
	if exactRun.UnresolvedToolApproval != "" {
		t.Fatalf("exact pause left unresolved marker %q", exactRun.UnresolvedToolApproval)
	}
	exactClaim := store.ToolApprovalClaim{InteractionID: "exact-pause", Token: "exact"}
	claimResume(exactClaim, exactRun, true)
	spend(exactClaim, true)
	if _, won, err := flipper.FlipToQueued(ctx, runID, store.RunStatusPausedWaitingHuman, time.Now()); err != nil || won {
		t.Fatalf("unacknowledged flip over an unresolved run: won=%v err=%v", won, err)
	}
	if err := s.PauseRun(ctx, runID, &store.Checkpoint{NodeID: "gate", InteractionID: "exact-pause", BackendPermissionInput: []byte(`{"label":"cyan"}`)}); err != nil {
		t.Fatal(err)
	}
	pausedAgain := load()
	if pausedAgain.UnresolvedToolApproval != "" {
		t.Fatalf("second exact pause left unresolved marker %q", pausedAgain.UnresolvedToolApproval)
	}
	if _, won, err := flipper.FlipAcknowledgedRevisionToQueued(ctx, runID, store.RunStatusPausedWaitingHuman, time.Now(), pausedAgain.CASVersion); err != nil || !won {
		t.Fatalf("acknowledged flip over an unresolved run: won=%v err=%v", won, err)
	}
}
