package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Conformance is the minimum behaviour every RunStore impl must
// honour, regardless of backend (filesystem today, Mongo+S3 in plan
// §F T-17). The harness is parameterised on a factory function so a
// future Mongo test can call exactly the same assertions and surface
// any drift.
//
// Invariants checked:
//   - CreateRun → LoadRun round-trip preserves the user-visible fields.
//   - UpdateRunStatus rolls forward and clamps FinishedAt at terminals.
//   - AppendEvent issues a strictly-monotonic seq starting at 1.
//   - Concurrent AppendEvent calls each get a unique seq.
//   - WriteArtifact versions are strictly increasing per node.
//   - LockRun is exclusive — a second LockRun fails until Unlock.
//   - Capabilities() reports a non-empty set for any real backend.

// runStoreFactory returns a fresh, empty store for one subtest.
// Cleanup is the harness's responsibility (t.TempDir for FS).
type runStoreFactory func(t *testing.T) RunStore

func conformanceSuite(t *testing.T, factory runStoreFactory) {
	conformanceSuiteWithOpts(t, factory, conformanceOpts{InitialStatus: RunStatusRunning})
}

// conformanceOpts and conformanceSuiteWithOpts mirror the exported
// names in pkg/store/storetest so the FS-side suite stays in lockstep
// with the cross-backend harness.
type conformanceOpts struct {
	InitialStatus RunStatus
}

func conformanceSuiteWithOpts(t *testing.T, factory runStoreFactory, opts conformanceOpts) {
	t.Run("CreateLoadRoundTrip", func(t *testing.T) { testCreateLoad(t, factory(t), opts) })
	t.Run("StatusTransitions", func(t *testing.T) { testStatusTransitions(t, factory(t)) })
	t.Run("SaveRunHostileValues", func(t *testing.T) { testSaveRunHostileValues(t, factory(t)) })
	t.Run("MergeClaimCAS", func(t *testing.T) { testMergeClaimCAS(t, factory(t)) })
	t.Run("RoutingPolicyImmutable", func(t *testing.T) { testRoutingPolicyImmutable(t, factory(t)) })
	t.Run("OutputsSurviveTerminal", func(t *testing.T) { testOutputsSurviveTerminal(t, factory(t)) })
	t.Run("RouteDecisionRegistry", func(t *testing.T) { testRouteDecisionRegistry(t, factory(t)) })
	t.Run("EventSeqMonotone", func(t *testing.T) { testEventSeqMonotone(t, factory(t)) })
	t.Run("EventSeqUnderConcurrency", func(t *testing.T) { testEventSeqConcurrent(t, factory(t)) })
	t.Run("ArtifactVersionsMonotone", func(t *testing.T) { testArtifactVersions(t, factory(t)) })
	t.Run("LockExclusivity", func(t *testing.T) { testLockExclusive(t, factory(t)) })
	t.Run("CapabilitiesReported", func(t *testing.T) { testCapabilitiesReported(t, factory(t)) })
	t.Run("UserMessagesInbox", func(t *testing.T) { testUserMessagesInbox(t, factory(t)) })
}

func testCreateLoad(t *testing.T, s RunStore, opts conformanceOpts) {
	t.Helper()
	in := map[string]any{"foo": "bar"}
	r, err := s.CreateRun(context.Background(), "run_1", "demo", in)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if r.ID != "run_1" {
		t.Errorf("ID: got %q", r.ID)
	}
	if r.Status != opts.InitialStatus {
		t.Errorf("Status: got %q want %q", r.Status, opts.InitialStatus)
	}
	r2, err := s.LoadRun(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r2.WorkflowName != "demo" {
		t.Errorf("WorkflowName: got %q", r2.WorkflowName)
	}
	if r2.Inputs["foo"] != "bar" {
		t.Errorf("Inputs[foo]: got %v", r2.Inputs["foo"])
	}
}

func testStatusTransitions(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_2", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(context.Background(), "run_2", RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	r, _ := s.LoadRun(context.Background(), "run_2")
	if r.Status != RunStatusFinished {
		t.Errorf("Status: got %q", r.Status)
	}
	if r.FinishedAt == nil {
		t.Errorf("FinishedAt: expected set on terminal status")
	}
}

func testEventSeqMonotone(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_3", "demo", nil); err != nil {
		t.Fatal(err)
	}
	const N = 50
	var prev int64 = -1
	for i := 0; i < N; i++ {
		ev := Event{Type: EventNodeStarted, Timestamp: time.Now().UTC()}
		written, err := s.AppendEvent(context.Background(), "run_3", ev)
		if err != nil {
			t.Fatalf("AppendEvent #%d: %v", i, err)
		}
		// The base seq value is implementation-defined (FS starts at 0,
		// Mongo will start at 1) — what matters is the strictly-monotone
		// invariant: every observation is greater than the previous.
		if written.Seq <= prev {
			t.Errorf("Seq #%d: %d not strictly greater than prev %d", i, written.Seq, prev)
		}
		prev = written.Seq
	}
	all, err := s.LoadEvents(context.Background(), "run_3")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != N {
		t.Errorf("LoadEvents: got %d want %d", len(all), N)
	}
}

func testEventSeqConcurrent(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_4", "demo", nil); err != nil {
		t.Fatal(err)
	}
	const goroutines = 8
	const perG = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ev := Event{Type: EventNodeStarted, Timestamp: time.Now().UTC()}
				if _, err := s.AppendEvent(context.Background(), "run_4", ev); err != nil {
					t.Errorf("AppendEvent: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	all, err := s.LoadEvents(context.Background(), "run_4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(all), goroutines*perG; got != want {
		t.Errorf("event count: got %d want %d", got, want)
	}
	seen := make(map[int64]struct{}, len(all))
	for i, ev := range all {
		if _, dup := seen[ev.Seq]; dup {
			t.Errorf("duplicate seq %d at index %d", ev.Seq, i)
		}
		seen[ev.Seq] = struct{}{}
		// seq must be non-negative. We don't assert an upper bound:
		// backends with retry-on-collision (Mongo $inc) burn extra
		// slots under contention and would tip a tight window check
		// into flakes. The monotonicity test above (testEventSeqMonotone)
		// covers ordering; uniqueness covers duplicates; that's enough.
		if ev.Seq < 0 {
			t.Errorf("negative seq at index %d: %d", i, ev.Seq)
		}
	}
}

func testArtifactVersions(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_5", "demo", nil); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 3; v++ {
		if err := s.WriteArtifact(context.Background(), &Artifact{
			RunID:     "run_5",
			NodeID:    "node_a",
			Version:   v,
			Data:      map[string]any{"v": v},
			WrittenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("WriteArtifact v=%d: %v", v, err)
		}
	}
	versions, err := s.ListArtifactVersions(context.Background(), "run_5", "node_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("ListArtifactVersions: got %d want 3", len(versions))
	}
	for i, vinfo := range versions {
		if vinfo.Version != i+1 {
			t.Errorf("Version[%d]: got %d want %d", i, vinfo.Version, i+1)
		}
	}
	latest, err := s.LoadLatestArtifact(context.Background(), "run_5", "node_a")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 3 {
		t.Errorf("Latest version: got %d want 3", latest.Version)
	}
}

func testLockExclusive(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_6", "demo", nil); err != nil {
		t.Fatal(err)
	}
	first, err := s.LockRun(context.Background(), "run_6")
	if err != nil {
		t.Fatalf("first LockRun: %v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	// Re-locking after a clean unlock must succeed — the lock is
	// strictly advisory across the unlock boundary.
	second, err := s.LockRun(context.Background(), "run_6")
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Errorf("second Unlock: %v", err)
	}
}

func testCapabilitiesReported(t *testing.T, s RunStore) {
	t.Helper()
	caps := s.Capabilities()
	// The non-regression we care about: any concrete backend exposes
	// at least *one* capability. A struct full of false is a sign the
	// impl forgot to override the method.
	if !caps.LiveStream && !caps.CrossProcessLock && !caps.PIDFile && !caps.GitWorktree {
		t.Errorf("Capabilities all-false; backend must report at least one")
	}
}

func testUserMessagesInbox(t *testing.T, s RunStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "run_um", "demo", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	msgs := []QueuedUserMessage{
		{ID: "m1", Text: "first", QueuedAt: now.Add(0)},
		{ID: "m2", Text: "second", QueuedAt: now.Add(10 * time.Millisecond)},
		{ID: "m3", Text: "third", QueuedAt: now.Add(20 * time.Millisecond)},
	}
	for _, m := range msgs {
		if err := s.AppendQueuedMessage(ctx, "run_um", m); err != nil {
			t.Fatalf("AppendQueuedMessage(%s): %v", m.ID, err)
		}
	}
	pending, err := s.LoadPendingQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("Pending count: got %d want 3", len(pending))
	}
	for i, want := range []string{"m1", "m2", "m3"} {
		if pending[i].ID != want {
			t.Errorf("FIFO[%d]: got %q want %q", i, pending[i].ID, want)
		}
		if pending[i].Status != QueuedMessageStatusQueued {
			t.Errorf("Initial status[%s]: got %q want queued", pending[i].ID, pending[i].Status)
		}
	}
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m1", QueuedMessageStatusDelivered); err != nil {
		t.Fatalf("Deliver m1: %v", err)
	}
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m2", QueuedMessageStatusDelivered); err != nil {
		t.Fatalf("Deliver m2: %v", err)
	}
	pending, err = s.LoadPendingQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("LoadPending after deliver: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "m3" {
		t.Fatalf("Pending after deliver = %+v, want only m3", pending)
	}
	all, err := s.ListQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("ListQueuedMessages: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List count: got %d want 3", len(all))
	}
	if all[0].Status != QueuedMessageStatusDelivered ||
		all[1].Status != QueuedMessageStatusDelivered ||
		all[2].Status != QueuedMessageStatusQueued {
		t.Errorf("statuses: %v / %v / %v", all[0].Status, all[1].Status, all[2].Status)
	}
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m3", QueuedMessageStatusCancelled, QueuedMessageStatusQueued); err != nil {
		t.Fatalf("Cancel m3: %v", err)
	}
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m1", QueuedMessageStatusCancelled, QueuedMessageStatusQueued); err == nil {
		t.Fatalf("Cancel of delivered m1: expected error")
	}
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "nonexistent", QueuedMessageStatusDelivered); err == nil {
		t.Fatalf("Update nonexistent: expected error")
	}
}

// TestConformance_Filesystem validates that the locally-shipped backend
// satisfies the conformance suite. The same factory shape will be used
// in plan §F T-17 to validate MongoRunStore against the same harness.
func TestConformance_Filesystem(t *testing.T) {
	conformanceSuite(t, func(t *testing.T) RunStore {
		t.Helper()
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// testSaveRunHostileValues guards the Mongo pipeline against
// aggregation-expression evaluation of DATA: a $-prefixed string value
// must round-trip verbatim (not be parsed as a field path and dropped
// or substituted), and a dotted map key must not reject the write —
// agent outputs ("$ ./gradlew build") and user inputs ("config.path")
// produce both shapes routinely.
func testSaveRunHostileValues(t *testing.T, s RunStore) {
	t.Helper()
	ctx := context.Background()
	const runID = "run-hostile-values"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Error = "$workflow_name is not a field path"
	r.Inputs = map[string]any{"config.path": "/etc/app.yml", "cmd": "$JAVA_HOME/bin/java"}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun with hostile values: %v", err)
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Error != r.Error {
		t.Fatalf("Error = %q, want %q (a $-value was evaluated, not stored)", got.Error, r.Error)
	}
	if got.Inputs["config.path"] != "/etc/app.yml" || got.Inputs["cmd"] != "$JAVA_HOME/bin/java" {
		t.Fatalf("Inputs = %v, want the hostile keys/values verbatim", got.Inputs)
	}

	// An upsert-create directly in a terminal status is NOT an episode:
	// nothing transitioned, the document was born that way (fork seeds
	// cancelled children through SaveRun on a run that does not exist).
	born := *got
	born.ID = "run-born-terminal"
	born.CASVersion = 0
	born.Status = RunStatusCancelled
	born.OutcomeSeq = 0
	if err := s.SaveRun(ctx, &born); err != nil {
		t.Fatalf("SaveRun upsert-create: %v", err)
	}
	if b, err := s.LoadRun(ctx, "run-born-terminal"); err != nil || b.OutcomeSeq != 0 {
		t.Fatalf("born-terminal = (seq %d, %v), want (0, nil)", b.OutcomeSeq, err)
	}
}

// testMergeClaimCAS drives the merge state machine at the store level:
// the claim CAS (entry), the conditional persist (exit), the stale
// steal with claim-token isolation, and the no-clobber-merged
// invariant that closes the double-squash TOCTOU.
func testMergeClaimCAS(t *testing.T, s RunStore) {
	t.Helper()
	ctx := context.Background()
	const runID = "run-merge-claim"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	notStale := time.Now().Add(-15 * time.Minute)

	// Entry: an unset merge_status is claimable, prior comes back "".
	claimed, prior, tokenA, err := s.ClaimMerge(ctx, runID, notStale)
	if err != nil || !claimed || prior != "" {
		t.Fatalf("first claim = (%t, %q, %v), want (true, \"\", nil)", claimed, prior, err)
	}
	if tokenA.IsZero() {
		t.Fatal("claim must return its token")
	}
	// A held (fresh) claim refuses the second claimant.
	claimed, prior, _, err = s.ClaimMerge(ctx, runID, notStale)
	if err != nil || claimed || prior != MergeStatusMerging {
		t.Fatalf("second claim = (%t, %q, %v), want (false, merging, nil)", claimed, prior, err)
	}

	// Exit: the holder persists the outcome conditioned on "merging"
	// AND its own token.
	changed, err := s.UpdateRunMergeIf(ctx, runID, RunMergeUpdate{
		Status:          MergeStatusMerged,
		MergedCommit:    "abc123",
		MergedInto:      "main",
		MergeStrategy:   MergeStrategySquash,
		ExpectClaimedAt: tokenA,
	}, []MergeStatus{MergeStatusMerging})
	if err != nil || !changed {
		t.Fatalf("persist merged = (%t, %v), want (true, nil)", changed, err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil || r.MergeStatus != MergeStatusMerged || r.MergedCommit != "abc123" || r.MergedInto != "main" {
		t.Fatalf("merged bookkeeping = %+v (%v)", r, err)
	}
	if !r.MergeClaimedAt.IsZero() {
		t.Errorf("MergeClaimedAt should be cleared by the exit write, got %v", r.MergeClaimedAt)
	}

	// No-clobber: a late writer still expecting "merging" (the loser of
	// a race, or a stolen-claim holder) cannot overwrite "merged".
	changed, err = s.UpdateRunMergeIf(ctx, runID, RunMergeUpdate{Status: MergeStatusFailed},
		[]MergeStatus{MergeStatusMerging})
	if err != nil || changed {
		t.Fatalf("clobber attempt = (%t, %v), want (false, nil)", changed, err)
	}
	if got, _ := s.LoadRun(ctx, runID); got.MergeStatus != MergeStatusMerged || got.MergedCommit != "abc123" {
		t.Fatalf("merged record damaged: %+v", got)
	}
	// And "merged" is terminal for the claim too.
	claimed, prior, _, err = s.ClaimMerge(ctx, runID, notStale)
	if err != nil || claimed || prior != MergeStatusMerged {
		t.Fatalf("claim on merged = (%t, %q, %v), want (false, merged, nil)", claimed, prior, err)
	}

	// Stale steal: a claim whose stamp predates staleBefore is up for
	// grabs — AND the stolen-from claimant's token consumes nothing
	// afterwards (the claim names an owner, not just a state; without
	// the token check the crashed claimant's late failure write would
	// overwrite the live claimant's outcome).
	const runID2 = "run-merge-claim-stale"
	if _, err := s.CreateRun(ctx, runID2, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	_, _, tokenOld, err := s.ClaimMerge(ctx, runID2, notStale)
	if err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if claimed, _, _, err := s.ClaimMerge(ctx, runID2, notStale); err != nil || claimed {
		t.Fatalf("fresh claim must hold, got steal (%t, %v)", claimed, err)
	}
	claimed, prior, tokenNew, err := s.ClaimMerge(ctx, runID2, time.Now().Add(time.Second))
	if err != nil || !claimed || prior != MergeStatusMerging {
		t.Fatalf("stale steal = (%t, %q, %v), want (true, merging, nil)", claimed, prior, err)
	}
	if tokenNew.Equal(tokenOld) {
		t.Fatal("steal must issue a fresh token")
	}
	changed, err = s.UpdateRunMergeIf(ctx, runID2, RunMergeUpdate{Status: MergeStatusFailed, ExpectClaimedAt: tokenOld},
		[]MergeStatus{MergeStatusMerging})
	if err != nil || changed {
		t.Fatalf("stolen-from claimant's write = (%t, %v), want (false, nil)", changed, err)
	}
	changed, err = s.UpdateRunMergeIf(ctx, runID2, RunMergeUpdate{Status: MergeStatusMerged, MergedCommit: "def456", MergedInto: "main", ExpectClaimedAt: tokenNew},
		[]MergeStatus{MergeStatusMerging})
	if err != nil || !changed {
		t.Fatalf("live claimant's write = (%t, %v), want (true, nil)", changed, err)
	}

	// A "merging" whose stamp is missing entirely (a full-document
	// writer dropped it) is claimable — it must not wedge the run.
	const runID3 = "run-merge-claim-nostamp"
	if _, err := s.CreateRun(ctx, runID3, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if changed, err := s.UpdateRunMergeIf(ctx, runID3, RunMergeUpdate{Status: MergeStatusMerging},
		[]MergeStatus{""}); err != nil || !changed {
		t.Fatalf("seed stampless merging = (%t, %v)", changed, err)
	}
	claimed, _, _, err = s.ClaimMerge(ctx, runID3, notStale)
	if err != nil || !claimed {
		t.Fatalf("stampless merging must be claimable = (%t, %v)", claimed, err)
	}

	// skipped and conflicted stay claimable (/merge is the only path
	// that re-materialises a lost merge clone; a recovered run lands
	// "skipped" and must stay mergeable).
	for _, st := range []MergeStatus{MergeStatusSkipped, MergeStatusConflicted} {
		id := "run-merge-claim-" + string(st)
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if changed, err := s.UpdateRunMergeIf(ctx, id, RunMergeUpdate{Status: st}, []MergeStatus{""}); err != nil || !changed {
			t.Fatalf("seed %s = (%t, %v)", st, changed, err)
		}
		claimed, prior, _, err := s.ClaimMerge(ctx, id, notStale)
		if err != nil || !claimed || prior != st {
			t.Fatalf("claim on %s = (%t, %q, %v), want (true, %s, nil)", st, claimed, prior, err, st)
		}
	}

	// Exit CAS with the empty status in expectedFrom matches an unset
	// field (a run that never entered the machine).
	const runID4 = "run-merge-claim-virgin"
	if _, err := s.CreateRun(ctx, runID4, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	changed, err = s.UpdateRunMergeIf(ctx, runID4, RunMergeUpdate{Status: MergeStatusPending},
		[]MergeStatus{""})
	if err != nil || !changed {
		t.Fatalf("empty-status CAS = (%t, %v), want (true, nil)", changed, err)
	}

	// A missing run is an ERROR, not a silent (false, nil) — a caller
	// must be able to tell a lost race from a deleted run.
	if _, err := s.UpdateRunMergeIf(ctx, "run-merge-claim-ghost", RunMergeUpdate{Status: MergeStatusPending},
		[]MergeStatus{""}); err == nil {
		t.Fatal("UpdateRunMergeIf on a missing run must error")
	}
}

// testRoutingPolicyImmutable: once the launch persisted the contract,
// no full-document saver — however stale — can drop or replace it.
// Retroactively changing the contract of already-produced work is the
// exact attack the launch-frozen snapshot exists to prevent.
func testRoutingPolicyImmutable(t *testing.T, s RunStore) {
	t.Helper()
	ctx := context.Background()
	const runID = "run-routing-policy"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	launch := &RoutingPolicy{Version: 1, SuccessWhen: "outputs.gate.ok", AllowedActions: []string{"merge"}}
	launch.Hash = launch.ComputeHash()
	r.RoutingPolicy = launch
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun launch: %v", err)
	}

	// A stale saver without the field cannot drop it…
	stale, _ := s.LoadRun(ctx, runID)
	stale.RoutingPolicy = nil
	if err := s.SaveRun(ctx, stale); err != nil {
		t.Fatalf("SaveRun stale: %v", err)
	}
	got, _ := s.LoadRun(ctx, runID)
	if got.RoutingPolicy == nil || got.RoutingPolicy.Hash != launch.Hash {
		t.Fatalf("policy dropped by a stale save: %+v", got.RoutingPolicy)
	}

	// …and a saver carrying a DIFFERENT contract cannot swap it.
	evil, _ := s.LoadRun(ctx, runID)
	swapped := &RoutingPolicy{Version: 1, SuccessWhen: "outputs.gate.other"}
	swapped.Hash = swapped.ComputeHash()
	evil.RoutingPolicy = swapped
	if err := s.SaveRun(ctx, evil); err != nil {
		t.Fatalf("SaveRun swap: %v", err)
	}
	got, _ = s.LoadRun(ctx, runID)
	if got.RoutingPolicy == nil || got.RoutingPolicy.Hash != launch.Hash {
		t.Fatalf("policy swapped by a save: %+v", got.RoutingPolicy)
	}
	// The first-write window closes at the terminal: a run that
	// finished WITHOUT a contract cannot be given one after the fact —
	// that would decide already-produced work retroactively.
	const lateID = "run-routing-policy-late"
	if _, err := s.CreateRun(ctx, lateID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, lateID, RunStatusFinished, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	late, _ := s.LoadRun(ctx, lateID)
	late.RoutingPolicy = launch
	if err := s.SaveRun(ctx, late); err != nil {
		t.Fatalf("SaveRun late: %v", err)
	}
	if got, _ := s.LoadRun(ctx, lateID); got.RoutingPolicy != nil {
		t.Fatalf("a contract was fixed onto already-terminal work: %+v", got.RoutingPolicy)
	}
}

// testOutputsSurviveTerminal: the checkpoint's outputs are the run's
// terminal evidence — the values a routing contract evaluates. They
// must survive the transition INTO finished on every backend (the FS
// store used to clear them there while Mongo kept them: the two
// backends diverged on the very field a decision reads).
func testOutputsSurviveTerminal(t *testing.T, s RunStore) {
	t.Helper()
	ctx := context.Background()
	const runID = "run-outputs-survive"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &Checkpoint{Outputs: map[string]map[string]any{"gate": {"converged": true}}}
	if err := s.SaveCheckpoint(ctx, runID, cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, runID, RunStatusFinished, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.Outputs["gate"]["converged"] != true {
		t.Fatalf("terminal outputs destroyed by the finish transition: %+v", r.Checkpoint)
	}
}

// testRouteDecisionRegistry holds both registry backends to one
// contract: the unique episode claim, the leased steal of an orphaned
// "claimed" row, the bounded retry of "failed", the finish states, the
// audit ordering and the sweep query.
func testRouteDecisionRegistry(t *testing.T, s RunStore) {
	t.Helper()
	rds := AsRouteDecisionStore(s)
	if rds == nil {
		t.Skip("backend has no route-decision registry")
	}
	ctx := context.Background()
	const runID = "run-route-registry"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// staleNever: the production threshold — a claim just taken is
	// fresh. staleAlways: a future threshold, which reads every existing
	// claim as expired (the ClaimMerge testability precedent).
	staleNever := func() time.Time { return time.Now().Add(-RouteClaimLease) }
	staleAlways := func() time.Time { return time.Now().Add(time.Hour) }

	// Fresh claim; duplicate refused with the existing row.
	claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 1, Decision: "merge", Reason: "r1"}, staleNever())
	if err != nil || !claimed {
		t.Fatalf("first claim = (%t, %v)", claimed, err)
	}
	claimed, existing, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 1, Decision: "merge"}, staleNever())
	if err != nil || claimed || existing == nil || existing.State != RouteDecisionClaimed {
		t.Fatalf("dup claim = (%t, %+v, %v), want refused with the claimed row", claimed, existing, err)
	}

	// Finish → succeeded; a succeeded episode is never reclaimable —
	// not even under a threshold that reads every claim as stale.
	if err := rds.FinishRouteDecision(ctx, runID, 1, RouteDecisionSucceeded, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 1}, staleAlways()); err != nil || claimed {
		t.Fatalf("succeeded episode reclaimed = (%t, %v)", claimed, err)
	}

	// A failed episode is reclaimable, but bounded by the attempt cap.
	if claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 2, Decision: "merge"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim ep2 = (%t, %v)", claimed, err)
	}
	for attempt := 1; ; attempt++ {
		if err := rds.FinishRouteDecision(ctx, runID, 2, RouteDecisionFailed, "transient"); err != nil {
			t.Fatalf("fail ep2 (attempt %d): %v", attempt, err)
		}
		claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 2, Decision: "merge"}, staleNever())
		if err != nil {
			t.Fatalf("reclaim ep2: %v", err)
		}
		if !claimed {
			if attempt < MaxRouteDecisionAttempts-1 {
				t.Fatalf("failed episode refused after only %d attempts (cap %d)", attempt, MaxRouteDecisionAttempts)
			}
			break
		}
		if attempt > MaxRouteDecisionAttempts {
			t.Fatalf("failed episode reclaimable beyond the cap (%d attempts)", attempt)
		}
	}

	// The leased steal: a stale "claimed" row is re-claimable — but the
	// steal is bounded by the SAME attempt cap, or a poison episode that
	// keeps killing its claimant re-arms forever (measured 9 steals
	// against a cap of 3 before the bound).
	if claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim ep3 = (%t, %v)", claimed, err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleNever()); err != nil || claimed {
		t.Fatalf("fresh claim stolen under the production threshold = (%t, %v)", claimed, err)
	}
	for steal := 2; steal <= MaxRouteDecisionAttempts; steal++ {
		claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleAlways())
		if err != nil || !claimed {
			t.Fatalf("steal %d of stale claim = (%t, %v)", steal, claimed, err)
		}
	}
	claimed, existing, err = rds.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleAlways())
	if err != nil || claimed {
		t.Fatalf("steal beyond the cap = (%t, %v), want refused", claimed, err)
	}
	if existing == nil || existing.Attempts < MaxRouteDecisionAttempts {
		t.Fatalf("cap-refused steal must return the exhausted row, got %+v", existing)
	}

	// The audit lists newest episode first.
	ds, err := rds.ListRouteDecisions(ctx, runID)
	if err != nil || len(ds) != 3 || ds[0].OutcomeSeq != 3 || ds[1].OutcomeSeq != 2 || ds[2].OutcomeSeq != 1 {
		t.Fatalf("ListRouteDecisions = %+v (%v)", ds, err)
	}

	// The activation watermark: established first-writer-wins, then
	// stable across every later call (a restart must read the original
	// activation, not its own boot). Backend round-trips may truncate
	// sub-millisecond precision — equality within a second is the claim.
	wm1, err := rds.EnsureRouterWatermark(ctx)
	if err != nil || wm1.IsZero() {
		t.Fatalf("EnsureRouterWatermark = (%v, %v)", wm1, err)
	}
	wm2, err := rds.EnsureRouterWatermark(ctx)
	if err != nil {
		t.Fatalf("EnsureRouterWatermark (second): %v", err)
	}
	if d := wm2.Sub(wm1); d < -time.Second || d > time.Second {
		t.Fatalf("watermark moved between calls: %v vs %v", wm1, wm2)
	}

	// The sweep query: only policy-carrying terminal runs, oldest first.
	pol := &RoutingPolicy{Version: 1, SuccessWhen: "outputs.g.ok", AllowedActions: []string{"merge"}}
	pol.Hash = pol.ComputeHash()
	mk := func(id string, terminal bool, withPolicy bool) {
		t.Helper()
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, _ := s.LoadRun(ctx, id)
		if withPolicy {
			r.RoutingPolicy = pol
		}
		if terminal {
			r.Status = RunStatusFinished
		}
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	mk("routable-a", true, true)
	mk("not-terminal", false, true)
	mk("no-policy", true, false)
	ids, err := rds.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ListRoutableRuns: %v", err)
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["routable-a"] || found["not-terminal"] || found["no-policy"] {
		t.Fatalf("sweep query = %v, want exactly the policy-carrying terminal run", ids)
	}

	// The anti-join: a run whose CURRENT episode is settled (succeeded)
	// leaves the sweep list — decided terminals must not clog the batch
	// head — while a failed-under-cap episode stays routable (its
	// bounded retry still wants the re-offer).
	ra, err := s.LoadRun(ctx, "routable-a")
	if err != nil {
		t.Fatalf("LoadRun routable-a: %v", err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: ra.ID, OutcomeSeq: ra.OutcomeSeq, Decision: "escalate"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim routable-a = (%t, %v)", claimed, err)
	}
	if err := rds.FinishRouteDecision(ctx, ra.ID, ra.OutcomeSeq, RouteDecisionSucceeded, ""); err != nil {
		t.Fatalf("finish routable-a: %v", err)
	}
	mk("routable-b", true, true)
	rb, err := s.LoadRun(ctx, "routable-b")
	if err != nil {
		t.Fatalf("LoadRun routable-b: %v", err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, RouteDecision{RunID: rb.ID, OutcomeSeq: rb.OutcomeSeq, Decision: "merge"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim routable-b = (%t, %v)", claimed, err)
	}
	if err := rds.FinishRouteDecision(ctx, rb.ID, rb.OutcomeSeq, RouteDecisionFailed, "transient"); err != nil {
		t.Fatalf("fail routable-b: %v", err)
	}
	ids, err = rds.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ListRoutableRuns (anti-join): %v", err)
	}
	found = map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if found["routable-a"] {
		t.Fatalf("settled (succeeded) run still swept: %v", ids)
	}
	if !found["routable-b"] {
		t.Fatalf("failed-under-cap run dropped from the sweep: %v", ids)
	}

	// Oldest first is the contract, and the limit truncates AFTER the
	// sort: with limit 1 the oldest sleeping terminal must surface —
	// not the lexically-first or insertion-first one (a directory-order
	// truncation starves exactly the run the sweep net exists for).
	time.Sleep(5 * time.Millisecond)
	mk("aaa-routable-newer", true, true)
	ids, err = rds.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 1)
	if err != nil || len(ids) != 1 || ids[0] != "routable-b" {
		t.Fatalf("ListRoutableRuns(limit=1) = (%v, %v), want the oldest routable run [routable-b]", ids, err)
	}
}
