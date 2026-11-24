package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// seedMergeBirthRun creates id, optionally enters the merge state machine at
// "pending" through the unanchored exit, and returns the run as stored.
func seedMergeBirthRun(t *testing.T, s store.RunStore, ctx context.Context, id string, pending bool) *store.Run {
	t.Helper()
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	if pending {
		if changed, err := s.UpdateRunMergeIf(ctx, id, store.RunMergeUpdate{Status: store.MergeStatusPending}, []store.MergeStatus{""}); err != nil || !changed {
			t.Fatalf("seed pending %s = (%t, %v)", id, changed, err)
		}
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	if r.BirthID == "" {
		t.Fatalf("fixture: %s was created without a birth", id)
	}
	return r
}

// replaceMergeBirthRun deletes original and recreates a successor under the
// same ID with the same merge state and a fresh birth.
func replaceMergeBirthRun(t *testing.T, s store.RunStore, ctx context.Context, original *store.Run, pending bool) *store.Run {
	t.Helper()
	if err := s.DeleteRun(ctx, original.ID); err != nil {
		t.Fatalf("DeleteRun %s: %v", original.ID, err)
	}
	if _, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("PruneDeletionMarkers: %v", err)
	}
	successor := seedMergeBirthRun(t, s, ctx, original.ID, pending)
	if successor.BirthID == original.BirthID {
		t.Fatalf("fixture: the successor reused birth %q", original.BirthID)
	}
	return successor
}

// requireMergeBirthState reloads id and requires the run born as birth to
// hold exactly the given merge state.
func requireMergeBirthState(t *testing.T, s store.RunStore, ctx context.Context, id, birth string, status store.MergeStatus, commit string, claimedAt time.Time) {
	t.Helper()
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("reload %s: %v", id, err)
	}
	if r.BirthID != birth {
		t.Fatalf("fixture: %s holds birth %q, want %q", id, r.BirthID, birth)
	}
	if r.MergeStatus != status || r.MergedCommit != commit || !r.MergeClaimedAt.Equal(claimedAt) {
		t.Fatalf("%s merge state = (status %q, commit %q, claimed_at %v), want (%q, %q, %v)",
			id, r.MergeStatus, r.MergedCommit, r.MergeClaimedAt, status, commit, claimedAt)
	}
}

// testMergeExitExpectedBirth: the merge exit door is anchored to the birth its
// caller loaded. A successor recreated under the same ID refuses an update
// expecting the original's birth — typed, nothing written — and accepts one
// expecting its own. A zero expectation is the legacy, unanchored exit.
func testMergeExitExpectedBirth(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	const id = "run-merge-birth-exit"
	a := seedMergeBirthRun(t, s, ctx, id, true)
	upd := store.RunMergeUpdate{
		Status:        store.MergeStatusMerged,
		MergedCommit:  "abc123",
		MergedInto:    "main",
		MergeStrategy: store.MergeStrategySquash,
		ExpectBirth:   store.ExpectBirthOf(a),
	}
	b := replaceMergeBirthRun(t, s, ctx, a, true)
	changed, err := s.UpdateRunMergeIf(ctx, id, upd, []store.MergeStatus{store.MergeStatusPending})
	requireMergeBirthState(t, s, ctx, id, b.BirthID, store.MergeStatusPending, "", time.Time{})
	if !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("exit expecting the original's birth on its successor = (%t, %v), want ErrRunDataIdentityChanged", changed, err)
	}
	if changed {
		t.Fatal("a refused exit reported a write")
	}

	upd.ExpectBirth = store.ExpectBirthOf(b)
	if changed, err = s.UpdateRunMergeIf(ctx, id, upd, []store.MergeStatus{store.MergeStatusPending}); err != nil || !changed {
		t.Fatalf("exit expecting the run's own birth = (%t, %v), want (true, nil)", changed, err)
	}
	requireMergeBirthState(t, s, ctx, id, b.BirthID, store.MergeStatusMerged, "abc123", time.Time{})

	// Legacy: the zero expectation lands on whatever run holds the ID.
	const legacyID = "run-merge-birth-exit-legacy"
	original := seedMergeBirthRun(t, s, ctx, legacyID, true)
	successor := replaceMergeBirthRun(t, s, ctx, original, true)
	if changed, err = s.UpdateRunMergeIf(ctx, legacyID, store.RunMergeUpdate{Status: store.MergeStatusSkipped},
		[]store.MergeStatus{store.MergeStatusPending}); err != nil || !changed {
		t.Fatalf("legacy unanchored exit = (%t, %v), want (true, nil)", changed, err)
	}
	requireMergeBirthState(t, s, ctx, legacyID, successor.BirthID, store.MergeStatusSkipped, "", time.Time{})
}

// testMergeClaimExpectedBirth: the merge entry door is anchored too, so a
// successor is never claimed — an exit-only anchor would leave it wedged in
// "merging" until the claim goes stale. The birth decides before the status.
func testMergeClaimExpectedBirth(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	const id = "run-merge-birth-claim"
	notStale := time.Now().Add(-15 * time.Minute)
	a := seedMergeBirthRun(t, s, ctx, id, false)
	expectA := store.ExpectBirthOf(a)
	b := replaceMergeBirthRun(t, s, ctx, a, false)
	claimed, prior, token, err := s.ClaimMerge(ctx, id, notStale, expectA)
	requireMergeBirthState(t, s, ctx, id, b.BirthID, "", "", time.Time{})
	if !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("claim expecting the original's birth on its successor = (%t, %q, %v, %v), want ErrRunDataIdentityChanged", claimed, prior, token, err)
	}
	if claimed || prior != "" || !token.IsZero() {
		t.Fatalf("a refused claim answered (%t, %q, %v)", claimed, prior, token)
	}

	claimed, prior, token, err = s.ClaimMerge(ctx, id, notStale, store.ExpectBirthOf(b))
	if err != nil || !claimed || prior != "" || token.IsZero() {
		t.Fatalf("claim expecting the run's own birth = (%t, %q, %v, %v), want (true, \"\", token, nil)", claimed, prior, token, err)
	}
	requireMergeBirthState(t, s, ctx, id, b.BirthID, store.MergeStatusMerging, "", token)

	// A held claim on the successor still answers the birth first: the
	// original's caller learns its run is gone, not that someone holds it.
	if claimed, prior, _, err = s.ClaimMerge(ctx, id, notStale, expectA); !errors.Is(err, store.ErrRunDataIdentityChanged) || claimed {
		t.Fatalf("claim expecting the original's birth on a claimed successor = (%t, %q, %v), want ErrRunDataIdentityChanged", claimed, prior, err)
	}
	requireMergeBirthState(t, s, ctx, id, b.BirthID, store.MergeStatusMerging, "", token)
}
