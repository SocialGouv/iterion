package boardmongo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// lastStatePayload returns the newest EvtIssueState payload for id — the
// provenance rows read the event the way pkg/trigger does.
func lastStatePayload(t *testing.T, s native.BoardStore, id string) map[string]any {
	t.Helper()
	var last map[string]any
	if err := s.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueState && e.IssueID == id {
			last = e.Payload
		}
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if last == nil {
		t.Fatalf("no state event for %s", id)
	}
	return last
}

// runBoardStoreSuite exercises the native.BoardStore contract. It runs against
// both the filesystem native.Store (always — proving the suite) and the Mongo
// store (gated on ITERION_TEST_MONGO_URI), so the two implementations are held
// to an identical bar.
func runBoardStoreSuite(t *testing.T, store native.BoardStore) {
	t.Helper()

	// Create: title required.
	if _, err := store.Create(native.Issue{}); err == nil {
		t.Error("Create without title should fail")
	}

	// Create defaults the state to the board's first state (inbox).
	created, err := store.Create(native.Issue{Title: "first", Labels: []string{"x"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.State != native.StateInbox {
		t.Errorf("default state: want %q, got %q", native.StateInbox, created.State)
	}
	if !strings.HasPrefix(created.ID, "native:") || created.CreatedAt.IsZero() {
		t.Errorf("created issue id/timestamps: %+v", created)
	}

	// Get found + not-found.
	if got, err := store.Get(created.ID); err != nil || got.Title != "first" {
		t.Errorf("Get: %+v err=%v", got, err)
	}
	if _, err := store.Get("native:00000000-0000-0000-0000-000000000000"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Get missing: want ErrNotFound, got %v", err)
	}

	// Resolve by bare uuid (no native: prefix).
	bare := strings.TrimPrefix(created.ID, "native:")
	if full, err := store.Resolve(bare); err != nil || full != created.ID {
		t.Errorf("Resolve(%q): %q err=%v", bare, full, err)
	}

	// Update: patch fields + no-op.
	pr := 5
	updated, err := store.Update(created.ID, native.Patch{Priority: &pr})
	if err != nil || updated.Priority != 5 {
		t.Errorf("Update priority: %+v err=%v", updated, err)
	}
	if _, err := store.Update(created.ID, native.Patch{Priority: &pr}); err != nil {
		t.Errorf("Update no-op: %v", err)
	}

	// set_bot via Update.Bot.
	bot := "feature-dev"
	if u, err := store.Update(created.ID, native.Patch{Bot: &bot}); err != nil || u.Bot != "feature-dev" {
		t.Errorf("Update bot: %+v err=%v", u, err)
	}

	// SetState: valid transition, unknown rejected, no-op same.
	if u, err := store.SetState(created.ID, native.StateReady); err != nil || u.State != native.StateReady {
		t.Errorf("SetState: %+v err=%v", u, err)
	}
	if _, err := store.SetState(created.ID, "does-not-exist"); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Errorf("SetState unknown: want ErrTransitionRejected, got %v", err)
	}
	if _, err := store.SetState(created.ID, native.StateReady); err != nil {
		t.Errorf("SetState no-op: %v", err)
	}

	// Claim: idempotent same marker; conflict on a different marker; release.
	if _, err := store.Claim(created.ID, "runner-A"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := store.Claim(created.ID, "runner-A"); err != nil {
		t.Errorf("Claim idempotent: %v", err)
	}
	if _, err := store.Claim(created.ID, "runner-B"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Errorf("Claim conflict: want ErrClaimConflict, got %v", err)
	}
	if err := store.Release(created.ID, "runner-B"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Errorf("Release by wrong marker: want ErrClaimConflict, got %v", err)
	}
	if err := store.Release(created.ID, "runner-A"); err != nil {
		t.Errorf("Release: %v", err)
	}
	if err := store.Release(created.ID, "runner-A"); err != nil {
		t.Errorf("Release unclaimed no-op: %v", err)
	}

	// --- Claim lease + fencing epoch (the watchdog substrate). Both
	// twins must hold the same bar: a fresh acquisition bumps the epoch
	// and stamps a lease; a same-marker re-claim refreshes the lease
	// WITHOUT bumping; release clears the lease but never the epoch; and
	// every Owned write under a superseded token is refused with the
	// issue left untouched — the fence that makes a stolen claim's late
	// writes no-ops.
	fenced, err := store.Create(native.Issue{Title: "fence probe", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create fence probe: %v", err)
	}
	tokA, err := store.Claim(fenced.ID, "owner-A")
	if err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	if tokA.Marker != "owner-A" || tokA.Epoch < 1 {
		t.Fatalf("claim token = %+v, want marker owner-A and epoch >= 1", tokA)
	}
	afterClaim, _ := store.Get(fenced.ID)
	if afterClaim.ClaimedAt.IsZero() || afterClaim.ClaimLeaseUntil.IsZero() {
		t.Fatalf("claim must stamp ClaimedAt + ClaimLeaseUntil: %+v", afterClaim)
	}
	if !afterClaim.ClaimLeaseUntil.After(afterClaim.ClaimedAt) {
		t.Fatalf("lease must expire after acquisition: %+v", afterClaim)
	}
	tokA2, err := store.Claim(fenced.ID, "owner-A")
	if err != nil || tokA2.Epoch != tokA.Epoch {
		t.Fatalf("same-marker re-claim = (%+v, %v), want same epoch %d", tokA2, err, tokA.Epoch)
	}
	if err := store.RenewClaim(fenced.ID, tokA); err != nil {
		t.Fatalf("RenewClaim: %v", err)
	}
	renewed, _ := store.Get(fenced.ID)
	if renewed.ClaimLeaseUntil.Before(afterClaim.ClaimLeaseUntil) {
		t.Fatalf("renew must not move the lease backwards: %v -> %v", afterClaim.ClaimLeaseUntil, renewed.ClaimLeaseUntil)
	}
	// Owned writes under the live token land.
	if _, err := store.SetStateOwned(fenced.ID, native.StateInProgress, tokA); err != nil {
		t.Fatalf("SetStateOwned (live token): %v", err)
	}
	if err := store.SetLastRunOwned(fenced.ID, "run-f1", "/tmp/f1", tokA); err != nil {
		t.Fatalf("SetLastRunOwned (live token): %v", err)
	}
	// The claim moves on: A releases, B acquires — the epoch must advance.
	if err := store.ReleaseOwned(fenced.ID, tokA); err != nil {
		t.Fatalf("ReleaseOwned: %v", err)
	}
	released, _ := store.Get(fenced.ID)
	if released.Claim != "" || !released.ClaimLeaseUntil.IsZero() || !released.ClaimedAt.IsZero() {
		t.Fatalf("release must clear the claim AND its lease bookkeeping: %+v", released)
	}
	if released.ClaimEpoch != tokA.Epoch {
		t.Fatalf("release must PRESERVE the epoch (the fence only moves forward): %+v vs token %d", released.ClaimEpoch, tokA.Epoch)
	}
	if err := store.ReleaseOwned(fenced.ID, tokA); err != nil {
		t.Fatalf("ReleaseOwned on unclaimed must be a no-op: %v", err)
	}
	tokB, err := store.Claim(fenced.ID, "owner-B")
	if err != nil {
		t.Fatalf("Claim B: %v", err)
	}
	if tokB.Epoch <= tokA.Epoch {
		t.Fatalf("fresh acquisition must advance the epoch: A=%d B=%d", tokA.Epoch, tokB.Epoch)
	}
	// Every late write under A's superseded token is refused, and the
	// issue is untouched.
	if _, err := store.SetStateOwned(fenced.ID, native.StateBlocked, tokA); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("SetStateOwned (stale token): want ErrClaimConflict, got %v", err)
	}
	if err := store.SetLastRunOwned(fenced.ID, "run-late", "/tmp/late", tokA); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("SetLastRunOwned (stale token): want ErrClaimConflict, got %v", err)
	}
	if err := store.SetAwaitingInputOwned(fenced.ID, true, tokA); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("SetAwaitingInputOwned (stale token): want ErrClaimConflict, got %v", err)
	}
	if err := store.SetGaveUpOwned(fenced.ID, &native.GiveUp{RunID: "run-late"}, tokA); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("SetGaveUpOwned (stale token): want ErrClaimConflict, got %v", err)
	}
	if err := store.RenewClaim(fenced.ID, tokA); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("RenewClaim (stale token): want ErrClaimConflict, got %v", err)
	}
	if err := store.ReleaseOwned(fenced.ID, tokA); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("ReleaseOwned (stale token on live foreign claim): want ErrClaimConflict, got %v", err)
	}
	after, _ := store.Get(fenced.ID)
	if after.State != native.StateInProgress || after.LastRunID != "run-f1" || after.AwaitingInput || after.GaveUp != nil || after.Claim != "owner-B" {
		t.Fatalf("stale-token writes must leave the issue untouched: %+v", after)
	}
	if err := store.ReleaseOwned(fenced.ID, tokB); err != nil {
		t.Fatalf("ReleaseOwned B: %v", err)
	}

	// --- Twin parity on the give-up buffer and on "unclaimed". The FS
	// store expires the give-up stamp on every write; the Mongo store's
	// targeted $set writers must do the same, or a card reads "the
	// dispatcher gave up" after something moved it on. And a claim field
	// that is ABSENT must read as free everywhere an empty one does —
	// nothing re-creates it, so a document that lost it would be
	// unclaimable, unlistable and invisible to the watchdog at once.
	gu, err := store.Create(native.Issue{Title: "give-up probe", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create give-up probe: %v", err)
	}
	guTok, err := store.Claim(gu.ID, "owner-gu")
	if err != nil {
		t.Fatalf("Claim give-up probe: %v", err)
	}
	if err := store.SetGaveUpOwned(gu.ID, &native.GiveUp{RunID: "run-gu", Attempts: 3}, guTok); err != nil {
		t.Fatalf("SetGaveUpOwned: %v", err)
	}
	if cur, _ := store.Get(gu.ID); cur.GaveUp == nil {
		t.Fatalf("the give-up stamp must land: %+v", cur)
	}
	if _, err := store.SetStateOwned(gu.ID, native.StateInProgress, guTok); err != nil {
		t.Fatalf("SetStateOwned after give-up: %v", err)
	}
	if cur, _ := store.Get(gu.ID); cur.GaveUp != nil {
		t.Fatalf("a state move must expire the give-up buffer (it describes the state it was taken in): %+v", cur.GaveUp)
	}
	if _, _, err := store.SetStateFrom(gu.ID, native.StateInProgress, native.StateReady); err != nil {
		t.Fatalf("SetStateFrom: %v", err)
	}
	// The EMPTY marker owns nothing: refused on a held card, and a silent
	// no-op on a free one. A conditional write filtered on `claim: marker`
	// would otherwise match an UNCLAIMED card and announce a release that
	// never happened — the twins must give the same two answers.
	if err := store.Release(gu.ID, ""); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("Release with an empty marker on a held card: want ErrClaimConflict, got %v", err)
	}
	if cur, _ := store.Get(gu.ID); cur.Claim != "owner-gu" {
		t.Fatalf("an empty-marker release must not touch the claim: %+v", cur.Claim)
	}
	if err := store.ReleaseOwned(gu.ID, guTok); err != nil {
		t.Fatalf("ReleaseOwned give-up probe: %v", err)
	}
	if err := store.Release(gu.ID, ""); err != nil {
		t.Fatalf("Release with an empty marker on a FREE card must be a silent no-op: %v", err)
	}
	if _, err := store.SetState(gu.ID, native.StateDone); err != nil {
		t.Fatalf("park give-up probe: %v", err)
	}

	// --- The reaper pair. A fresh lease is never listed nor reclaimable;
	// an expired one (probed with a future cutoff — the staleBefore
	// testability precedent) is TRANSFERRED, epoch bumped, old owner
	// fenced. Cutoff is the caller's: production passes now.
	reapProbe, err := store.Create(native.Issue{Title: "reap probe", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create reap probe: %v", err)
	}
	tokC, err := store.Claim(reapProbe.ID, "dead-owner")
	if err != nil {
		t.Fatalf("Claim reap probe: %v", err)
	}
	fresh, err := store.ListExpiredClaimCandidates(time.Now(), 50)
	if err != nil {
		t.Fatalf("ListExpiredClaimCandidates (fresh): %v", err)
	}
	for _, cand := range fresh {
		if cand.IssueID == reapProbe.ID {
			t.Fatalf("a FRESH lease must never be listed as expired: %+v", cand)
		}
	}
	future := time.Now().Add(2 * native.ClaimLeaseDuration)
	expired, err := store.ListExpiredClaimCandidates(future, 50)
	if err != nil {
		t.Fatalf("ListExpiredClaimCandidates (future cutoff): %v", err)
	}
	var probeCand *tracker.ExpiredClaim
	for i := range expired {
		if expired[i].IssueID == reapProbe.ID {
			probeCand = &expired[i]
		}
	}
	if probeCand == nil || probeCand.Prev.Marker != "dead-owner" || probeCand.Prev.Epoch != tokC.Epoch {
		t.Fatalf("expired listing must carry the claim as-is: %+v", probeCand)
	}
	// Wrong prev → conflict, nothing moves.
	if _, _, err := store.ReclaimExpired(reapProbe.ID, tracker.ClaimToken{Marker: "dead-owner", Epoch: tokC.Epoch + 7}, "reaper:x", future); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("reclaim with wrong prev: want ErrClaimConflict, got %v", err)
	}
	// Fresh cutoff → the lease is not expired → refused.
	if _, _, err := store.ReclaimExpired(reapProbe.ID, tokC, "reaper:x", time.Now()); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("reclaim of a live lease: want ErrClaimConflict, got %v", err)
	}
	// The real transfer: epoch bumps, the dead owner is fenced out.
	rec, recState, err := store.ReclaimExpired(reapProbe.ID, tokC, "reaper:x", future)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	// The contract is MONOTONICITY, not a unit increment: the Mongo twin
	// floors the counter at the server clock so a re-mint after the field
	// was dropped still lands ahead of every token ever issued, while the
	// FS twin (which never loses the field) simply increments. Both must
	// only ever move the fence FORWARD.
	if rec.Marker != "reaper:x" || rec.Epoch <= tokC.Epoch {
		t.Fatalf("transfer token = %+v, want reaper:x at an epoch strictly above %d", rec, tokC.Epoch)
	}
	// The transfer reports the state it OBSERVED: the watchdog decides a
	// card's disposition on this, never on the listing's older copy.
	if recState != native.StateInProgress {
		t.Fatalf("transfer must report the state it observed: %q, want %q", recState, native.StateInProgress)
	}
	if _, err := store.SetStateOwned(reapProbe.ID, native.StateBlocked, tokC); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("dead owner's write after the transfer: want ErrClaimConflict, got %v", err)
	}
	// The dead owner's TOKENLESS paths must die at the fence too — they
	// are the ones a stale in-flight worker still reaches. Release is
	// marker-scoped by contract, and an ORDINARY (unfenced) write must
	// never carry the claim family along: a read-modify-write that
	// re-persists a stale claim rewinds the fence and hands the card
	// back to the owner the reaper just evicted.
	if err := store.Release(reapProbe.ID, "dead-owner"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("Release by the evicted owner: want ErrClaimConflict, got %v", err)
	}
	if err := store.SetLastRun(reapProbe.ID, "run-stale", "/tmp/stale"); err != nil {
		t.Fatalf("SetLastRun (ordinary write): %v", err)
	}
	touchedTitle := "touched"
	if _, err := store.Update(reapProbe.ID, native.Patch{Title: &touchedTitle}); err != nil {
		t.Fatalf("Update (ordinary write): %v", err)
	}
	if _, _, err := store.AddComment(reapProbe.ID, "op", "a note"); err != nil {
		t.Fatalf("AddComment (ordinary write): %v", err)
	}
	held, _ := store.Get(reapProbe.ID)
	if held.Claim != "reaper:x" || held.ClaimEpoch != rec.Epoch || held.ClaimLeaseUntil.IsZero() {
		t.Fatalf("ordinary writes must not touch the claim family: claim=%q epoch=%d (want reaper:x epoch %d), lease=%s",
			held.Claim, held.ClaimEpoch, rec.Epoch, held.ClaimLeaseUntil)
	}
	if err := store.RenewClaim(reapProbe.ID, rec); err != nil {
		t.Fatalf("the recovery owner must still hold its card after ordinary writes: %v", err)
	}
	if _, err := store.SetStateOwned(reapProbe.ID, native.StateDone, rec); err != nil {
		t.Fatalf("recovery owner's write: %v", err)
	}
	if err := store.ReleaseOwned(reapProbe.ID, rec); err != nil {
		t.Fatalf("ReleaseOwned recovery: %v", err)
	}

	// --- Terminal sink + Reopen + SetStateFrom. Both twins: leaving a
	// Terminal:true state via the ordinary family is refused (typed,
	// wrapping ErrTransitionRejected so old callers still match); Reopen
	// is the one exit, working-state targets only, refused while
	// dependents promoted on this card's DONE are outstanding; the CAS
	// move reports drift instead of clobbering.
	sink, err := store.Create(native.Issue{Title: "sink probe", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create sink probe: %v", err)
	}
	if _, err := store.SetState(sink.ID, native.StateDone); err != nil {
		t.Fatalf("SetState(done): %v", err)
	}
	if _, err := store.SetState(sink.ID, native.StateReady); !errors.Is(err, tracker.ErrTerminalStateExit) || !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("terminal exit via SetState: want ErrTerminalStateExit (wrapping ErrTransitionRejected), got %v", err)
	}
	// The OWNED family is automation too — holding the claim is not a
	// licence to resurrect a card an operator closed. This is the exact
	// call the cloud launch path makes (in_progress under the token), so
	// a twin that skips the guard resurrects closed cards and runs on
	// them.
	sinkTok, err := store.Claim(sink.ID, "owner-sink")
	if err != nil {
		t.Fatalf("Claim sink probe: %v", err)
	}
	if _, err := store.SetStateOwned(sink.ID, native.StateInProgress, sinkTok); !errors.Is(err, tracker.ErrTerminalStateExit) {
		t.Fatalf("terminal exit via SetStateOwned: want ErrTerminalStateExit, got %v", err)
	}
	if cur, _ := store.Get(sink.ID); cur.State != native.StateDone {
		t.Fatalf("a refused owned exit must leave the card terminal, got %q", cur.State)
	}
	if err := store.ReleaseOwned(sink.ID, sinkTok); err != nil {
		t.Fatalf("ReleaseOwned sink probe: %v", err)
	}
	dependent, err := store.Create(native.Issue{Title: "dependent", State: native.StateReady, Blockers: []string{sink.ID}})
	if err != nil {
		t.Fatalf("Create dependent: %v", err)
	}
	if _, err := store.Reopen(sink.ID, native.StateReady); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("Reopen with a promoted dependent: want refusal, got %v", err)
	}
	if _, err := store.Update(dependent.ID, native.Patch{Blockers: &[]string{}}); err != nil {
		t.Fatalf("clear dependent blockers: %v", err)
	}
	if _, err := store.Reopen(sink.ID, native.StateBlocked); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("Reopen into another terminal: want refusal, got %v", err)
	}
	reopened, err := store.Reopen(sink.ID, native.StateReady)
	if err != nil || reopened.State != native.StateReady {
		t.Fatalf("Reopen = (%+v, %v), want ready", reopened, err)
	}
	if _, err := store.Reopen(sink.ID, native.StateInbox); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("Reopen of a non-terminal card: want refusal, got %v", err)
	}
	// SetStateFrom: lands on the expected source, reports drift otherwise.
	if _, changed, err := store.SetStateFrom(sink.ID, native.StateReady, native.StateInProgress); err != nil || !changed {
		t.Fatalf("SetStateFrom(ready→in_progress) = (changed=%t, %v)", changed, err)
	}
	if _, changed, err := store.SetStateFrom(sink.ID, native.StateReady, native.StateInbox); err != nil || changed {
		t.Fatalf("SetStateFrom on drifted state = (changed=%t, %v), want a clean no-op", changed, err)
	}
	if _, _, err := store.SetStateFrom(sink.ID, native.StateDone, native.StateReady); !errors.Is(err, tracker.ErrTerminalStateExit) {
		t.Fatalf("SetStateFrom out of terminal: want ErrTerminalStateExit, got %v", err)
	}
	// Park the probes terminally so the list-filter section below keeps
	// its counts (one shared store per suite run).
	if _, err := store.SetState(sink.ID, native.StateDone); err != nil {
		t.Fatalf("park sink probe: %v", err)
	}
	if _, err := store.SetState(dependent.ID, native.StateDone); err != nil {
		t.Fatalf("park dependent probe: %v", err)
	}

	// SetLastRun stamps the single pointer AND appends dedup'd run history.
	if err := store.SetLastRun(created.ID, "run-1", "/tmp/wd"); err != nil {
		t.Errorf("SetLastRun: %v", err)
	}
	if got, _ := store.Get(created.ID); got.LastRunID != "run-1" {
		t.Errorf("SetLastRun not persisted: %+v", got)
	}
	// A second, distinct run id appends a second RunRef (newest-last).
	if err := store.SetLastRun(created.ID, "run-2", "/tmp/wd2"); err != nil {
		t.Errorf("SetLastRun run-2: %v", err)
	}
	if got, _ := store.Get(created.ID); len(got.Runs) != 2 ||
		got.Runs[0].RunID != "run-1" || got.Runs[1].RunID != "run-2" {
		t.Errorf("run history not appended newest-last: %+v", got.Runs)
	}
	// Re-stamping an existing run id updates it in place, no growth.
	if err := store.SetLastRun(created.ID, "run-1", "/tmp/wd-moved"); err != nil {
		t.Errorf("SetLastRun run-1 re-stamp: %v", err)
	}
	if got, _ := store.Get(created.ID); len(got.Runs) != 2 || got.Runs[0].Workdir != "/tmp/wd-moved" {
		t.Errorf("run history dedup-update failed: %+v", got.Runs)
	}

	// SetAwaitingInput denormalizes the pause hint onto the card; set true,
	// clear false, with parity to the native store (idempotent, tagged).
	if err := store.SetAwaitingInput(created.ID, true); err != nil {
		t.Errorf("SetAwaitingInput(true): %v", err)
	}
	if got, _ := store.Get(created.ID); !got.AwaitingInput {
		t.Errorf("SetAwaitingInput(true) not persisted: %+v", got)
	}
	if err := store.SetAwaitingInput(created.ID, false); err != nil {
		t.Errorf("SetAwaitingInput(false): %v", err)
	}
	if got, _ := store.Get(created.ID); got.AwaitingInput {
		t.Errorf("SetAwaitingInput(false) not cleared: %+v", got)
	}

	// SetGaveUp records / clears the dispatcher's give-up stamp — the record
	// that distinguishes an automatic give-up from an operator filing the
	// same terminal state by hand. Both stores must round-trip the nested
	// value, or the pipeline board's needs-attention lane is cloud-blind.
	current, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before SetGaveUp: %v", err)
	}
	if err := store.SetGaveUp(created.ID, &native.GiveUp{RunID: "run-1", State: current.State, Attempts: 3}); err != nil {
		t.Errorf("SetGaveUp: %v", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Errorf("Get after SetGaveUp: %v", err)
	} else if got.GaveUp == nil || got.GaveUp.RunID != "run-1" || got.GaveUp.Attempts != 3 || got.GaveUp.At.IsZero() {
		t.Errorf("give-up stamp not persisted: %+v", got.GaveUp)
	} else if !got.GaveUp.Current(got.State, "run-1") {
		t.Errorf("stamp does not describe the issue it was written on: state=%q stamp=%+v", got.State, got.GaveUp)
	}
	// Moving the ticket expires the stamp for good — both stores enforce it
	// on their write path, or a returning ticket would resurrect a give-up.
	if _, err := store.SetState(created.ID, native.StateBlocked); err != nil {
		t.Errorf("SetState(blocked): %v", err)
	}
	if got, _ := store.Get(created.ID); got.GaveUp != nil {
		t.Errorf("stamp survived the ticket moving: %+v", got.GaveUp)
	}
	// Coming BACK from blocked is a terminal exit — the ordinary move is
	// refused by the sink guard, and Reopen is the sanctioned path.
	if _, err := store.SetState(created.ID, current.State); !errors.Is(err, tracker.ErrTerminalStateExit) {
		t.Errorf("SetState out of blocked: want ErrTerminalStateExit, got %v", err)
	}
	if _, err := store.Reopen(created.ID, current.State); err != nil {
		t.Errorf("Reopen(back): %v", err)
	}
	if got, _ := store.Get(created.ID); got.GaveUp != nil {
		t.Errorf("stamp came back when the ticket returned to its state: %+v", got.GaveUp)
	}
	// And an explicit clear is a no-op on an unstamped issue.
	if err := store.SetGaveUp(created.ID, nil); err != nil {
		t.Errorf("SetGaveUp(nil): %v", err)
	}
	if got, _ := store.Get(created.ID); got.GaveUp != nil {
		t.Errorf("SetGaveUp(nil) did not clear: %+v", got.GaveUp)
	}

	// List: filter by state + assignee; sort by priority.
	_, _ = store.Create(native.Issue{Title: "second", State: native.StateReady, Priority: 9})
	ready, err := store.List(native.ListFilter{States: []string{native.StateReady}})
	if err != nil || len(ready) != 2 {
		t.Errorf("List by state: got %d err=%v", len(ready), err)
	}
	if len(ready) == 2 && ready[0].Priority < ready[1].Priority {
		t.Errorf("List should sort by priority desc: %d then %d", ready[0].Priority, ready[1].Priority)
	}

	// AggregateLabels.
	labels := store.AggregateLabels()
	found := false
	for _, l := range labels {
		if l.Label == "x" && l.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("AggregateLabels missing label x: %+v", labels)
	}

	// ScanEvents: at least the create/update/state events landed.
	var n int
	if err := store.ScanEvents(func(*native.Event) bool { n++; return true }); err != nil {
		t.Errorf("ScanEvents: %v", err)
	}
	if n == 0 {
		t.Error("ScanEvents returned no events")
	}

	// Delete.
	if err := store.Delete(created.ID); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, err := store.Get(created.ID); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Get after delete: want ErrNotFound, got %v", err)
	}
	if err := store.Delete(created.ID); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Delete missing: want ErrNotFound, got %v", err)
	}

	// Claim provenance is a CROSS-TWIN contract read by pkg/trigger
	// (machineCaused refuses to spend a one-shot label gate; board_source
	// blanks the Actor). Two halves, both on both twins — the provenance
	// describes the WRITER, never whoever holds the card:
	//
	//	a watchdog's FENCED write     → reason: watchdog
	//	an OPERATOR's tokenless write → no reason, whoever holds the card
	wcard, err := store.Create(native.Issue{Title: "prov watchdog write", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("provenance create: %v", err)
	}
	wtok, err := store.Claim(wcard.ID, tracker.ReaperMarkerPrefix+"prov-host")
	if err != nil {
		t.Fatalf("provenance claim: %v", err)
	}
	if _, err := store.SetStateOwned(wcard.ID, native.StateBlocked, wtok); err != nil {
		t.Fatalf("provenance SetStateOwned: %v", err)
	}
	if got, _ := lastStatePayload(t, store, wcard.ID)["reason"].(string); got != tracker.ReasonWatchdog {
		t.Errorf("a watchdog's fenced move must be stamped %q, got %q — the spine would spend an operator's one-shot on a machine repair",
			tracker.ReasonWatchdog, got)
	}
	ocard, err := store.Create(native.Issue{Title: "prov operator write", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("provenance create: %v", err)
	}
	if _, err := store.Claim(ocard.ID, tracker.ReaperMarkerPrefix+"prov-host"); err != nil {
		t.Fatalf("provenance claim: %v", err)
	}
	if _, err := store.SetState(ocard.ID, native.StateReady); err != nil {
		t.Fatalf("provenance operator SetState: %v", err)
	}
	if got, _ := lastStatePayload(t, store, ocard.ID)["reason"].(string); got == tracker.ReasonWatchdog {
		t.Errorf("an OPERATOR move was stamped %q — trigger.machineCaused would refuse to spend the one-shot label gate the operator just pulled", got)
	}

	// Third provenance row — the auto-promote CASCADE. Both twins must
	// stamp tracker.ReasonUnblocked on the promoted card's state event:
	// the FS twin stamped and the Mongo twin did not, and the spine read
	// two different truths from one close (the reason is DESCRIPTIVE, not
	// machine — IsMachineReason excludes it, so the one-shot still fires).
	pblk, err := store.Create(native.Issue{Title: "prov promote blocker", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("provenance create blocker: %v", err)
	}
	pdep, err := store.Create(native.Issue{Title: "prov promote dependent", State: native.StateWaitingDeps, Blockers: []string{pblk.ID}})
	if err != nil {
		t.Fatalf("provenance create dependent: %v", err)
	}
	if _, err := store.SetState(pblk.ID, native.StateDone); err != nil {
		t.Fatalf("provenance close blocker: %v", err)
	}
	if dep, err := store.Get(pdep.ID); err != nil || dep.State == native.StateWaitingDeps {
		t.Fatalf("dependent not promoted (state=%v err=%v)", dep, err)
	}
	if got, _ := lastStatePayload(t, store, pdep.ID)["reason"].(string); got != tracker.ReasonUnblocked {
		t.Errorf("an auto-promoted card's state event must carry %q, got %q — the twins diverge and the spine reads two truths from one close",
			tracker.ReasonUnblocked, got)
	}
}

// runBoardAdminSuite exercises the native.BoardAdmin config-mutation surface
// (columns, fields, views, label vocabulary) plus the cascades to issues. It
// runs against both the filesystem native.Store and the Mongo store so the
// two implementations are held to an identical bar — same validation, same
// sentinel errors, same touched counts. `store` and `admin` are the SAME
// backing store viewed through both interfaces.
func runBoardAdminSuite(t *testing.T, store native.BoardStore, admin native.BoardAdmin) {
	t.Helper()

	// --- states (columns) ---

	if err := admin.AddState(native.State{Name: "triage", Display: "Triage"}); err != nil {
		t.Fatalf("AddState: %v", err)
	}
	if store.Board().StateByName("triage") == nil {
		t.Fatal("AddState: triage not persisted")
	}
	if err := admin.AddState(native.State{Name: "triage"}); err == nil {
		t.Error("AddState duplicate should fail")
	}
	if err := admin.AddState(native.State{Name: ""}); err == nil {
		t.Error("AddState empty name should fail")
	}

	// UpdateState: flip eligible + set display; never renames.
	yes := true
	if err := admin.UpdateState("triage", native.StatePatch{Eligible: &yes, Display: ptr("Triage!")}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if st := store.Board().StateByName("triage"); st == nil || !st.Eligible || st.Display != "Triage!" {
		t.Errorf("UpdateState not applied: %+v", st)
	}
	if err := admin.UpdateState("nope", native.StatePatch{Display: ptr("x")}); err == nil {
		t.Error("UpdateState unknown should fail")
	}

	// Park an issue in triage so RenameState/DeleteState cascade.
	parked, err := store.Create(native.Issue{Title: "parked", State: "triage"})
	if err != nil {
		t.Fatalf("Create parked: %v", err)
	}
	// RenameState cascades the card.
	n, err := admin.RenameState("triage", "triaging")
	if err != nil || n != 1 {
		t.Errorf("RenameState: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(parked.ID); got.State != "triaging" {
		t.Errorf("RenameState cascade: parked state=%q want triaging", got.State)
	}
	if store.Board().StateByName("triage") != nil {
		t.Error("RenameState: old column still present")
	}
	if _, err := admin.RenameState("triaging", native.StateInbox); err == nil {
		t.Error("RenameState onto existing column should fail")
	}
	if n, err := admin.RenameState("triaging", "triaging"); err != nil || n != 0 {
		t.Errorf("RenameState self no-op: touched=%d err=%v", n, err)
	}

	// DeleteState: non-empty without migrate target → ErrStateNotEmpty.
	if _, err := admin.DeleteState("triaging", ""); !errors.Is(err, native.ErrStateNotEmpty) {
		t.Errorf("DeleteState non-empty: want ErrStateNotEmpty, got %v", err)
	}
	// DeleteState with migrate target moves the card and drops the column.
	n, err = admin.DeleteState("triaging", native.StateBacklog)
	if err != nil || n != 1 {
		t.Errorf("DeleteState migrate: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(parked.ID); got.State != native.StateBacklog {
		t.Errorf("DeleteState migrate: parked state=%q want backlog", got.State)
	}
	if store.Board().StateByName("triaging") != nil {
		t.Error("DeleteState: column still present")
	}
	if _, err := admin.DeleteState("ghost", ""); err == nil {
		t.Error("DeleteState unknown should fail")
	}

	// Deleting a TERMINAL column into a working one reopens every card in
	// it at once. Both twins must hold it to the same bar as the
	// single-card Reopen — otherwise the column editor is the way around
	// the sink on whichever twin forgot the guard.
	closed, err := store.Create(native.Issue{Title: "closed with a dependent", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	if _, err := store.SetState(closed.ID, native.StateDone); err != nil {
		t.Fatalf("SetState done: %v", err)
	}
	dependent, err := store.Create(native.Issue{Title: "promoted dependent", State: native.StateReady, Blockers: []string{closed.ID}})
	if err != nil {
		t.Fatalf("Create dependent: %v", err)
	}
	if _, err := admin.DeleteState(native.StateDone, native.StateReady); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Errorf("deleting a terminal column into a working one is a bulk reopen: want the dependents refusal, got %v", err)
	}
	if got, _ := store.Get(closed.ID); got.State != native.StateDone {
		t.Errorf("a refused bulk reopen must leave its cards terminal, got %q", got.State)
	}
	// Cleared, it proceeds — the guard refuses a class, not the gesture.
	if _, err := store.Update(dependent.ID, native.Patch{Blockers: &[]string{}}); err != nil {
		t.Fatalf("clear blockers: %v", err)
	}
	if _, err := admin.DeleteState(native.StateDone, native.StateReady); err != nil {
		t.Errorf("with no promoted dependents the migration must proceed: %v", err)
	}
	if err := admin.AddState(native.State{Name: native.StateDone, Display: "Done", Terminal: true}); err != nil {
		t.Fatalf("restore done column: %v", err)
	}

	// ReorderStates: permutation only.
	cur := store.Board()
	names := make([]string, len(cur.States))
	for i, st := range cur.States {
		names[i] = st.Name
	}
	if len(names) >= 2 {
		swapped := append([]string(nil), names...)
		swapped[0], swapped[1] = swapped[1], swapped[0]
		if err := admin.ReorderStates(swapped); err != nil {
			t.Errorf("ReorderStates: %v", err)
		}
		if store.Board().States[0].Name != swapped[0] {
			t.Errorf("ReorderStates not applied: %+v", store.Board().States)
		}
	}
	if err := admin.ReorderStates([]string{"only-one"}); err == nil {
		t.Error("ReorderStates non-permutation should fail")
	}

	// --- fields ---

	if err := admin.AddField(native.Field{Name: "severity", Type: native.FieldText}); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if store.Board().FieldByName("severity") == nil {
		t.Fatal("AddField: severity not persisted")
	}
	if err := admin.AddField(native.Field{Name: "severity", Type: native.FieldText}); err == nil {
		t.Error("AddField duplicate should fail")
	}
	if err := admin.AddField(native.Field{Name: "bad", Type: native.FieldEnum}); err == nil {
		t.Error("AddField enum without values should fail board validation")
	}

	// UpdateField: change display/required in place.
	if err := admin.UpdateField("severity", native.FieldPatch{Display: ptr("Severity")}); err != nil {
		t.Errorf("UpdateField: %v", err)
	}
	if f := store.Board().FieldByName("severity"); f == nil || f.Display != "Severity" {
		t.Errorf("UpdateField not applied: %+v", f)
	}
	if err := admin.UpdateField("nope", native.FieldPatch{Display: ptr("x")}); err == nil {
		t.Error("UpdateField unknown should fail")
	}

	// Put a value on an issue so RenameField/DeleteField cascade.
	withField, err := store.Create(native.Issue{Title: "has-field", Fields: map[string]any{"severity": "high"}})
	if err != nil {
		t.Fatalf("Create withField: %v", err)
	}
	// RenameField cascades the key.
	n, err = admin.RenameField("severity", "sev")
	if err != nil || n != 1 {
		t.Errorf("RenameField: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(withField.ID); got.Fields["sev"] != "high" || got.Fields["severity"] != nil {
		t.Errorf("RenameField cascade: fields=%+v", got.Fields)
	}
	if store.Board().FieldByName("severity") != nil {
		t.Error("RenameField: old field def still present")
	}
	if _, err := admin.RenameField("sev", "bot_args"); err == nil {
		t.Error("RenameField onto existing field should fail")
	}
	// DeleteField strips the key.
	n, err = admin.DeleteField("sev")
	if err != nil || n != 1 {
		t.Errorf("DeleteField: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(withField.ID); got.Fields["sev"] != nil {
		t.Errorf("DeleteField cascade: key not stripped: %+v", got.Fields)
	}
	if store.Board().FieldByName("sev") != nil {
		t.Error("DeleteField: field def still present")
	}
	if _, err := admin.DeleteField("ghost"); err == nil {
		t.Error("DeleteField unknown should fail")
	}

	// ReorderFields: add a second field, then permute.
	if err := admin.AddField(native.Field{Name: "owner", Type: native.FieldText}); err != nil {
		t.Fatalf("AddField owner: %v", err)
	}
	fcur := store.Board()
	fnames := make([]string, len(fcur.Fields))
	for i, f := range fcur.Fields {
		fnames[i] = f.Name
	}
	if len(fnames) >= 2 {
		rev := make([]string, len(fnames))
		for i := range fnames {
			rev[i] = fnames[len(fnames)-1-i]
		}
		if err := admin.ReorderFields(rev); err != nil {
			t.Errorf("ReorderFields: %v", err)
		}
		if store.Board().Fields[0].Name != rev[0] {
			t.Errorf("ReorderFields not applied: %+v", store.Board().Fields)
		}
	}
	if err := admin.ReorderFields([]string{"x"}); err == nil {
		t.Error("ReorderFields non-permutation should fail")
	}

	// --- views ---

	if err := admin.SaveView(native.View{Name: "mine", Assignee: "me"}); err != nil {
		t.Fatalf("SaveView: %v", err)
	}
	if err := admin.SaveView(native.View{Name: "mine", Assignee: "you"}); err != nil {
		t.Fatalf("SaveView upsert: %v", err)
	}
	if vs := store.Board().Views; len(vs) != 1 || vs[0].Assignee != "you" {
		t.Errorf("SaveView upsert by name: %+v", vs)
	}
	if err := admin.SaveView(native.View{Name: ""}); err == nil {
		t.Error("SaveView empty name should fail")
	}
	if err := admin.DeleteView("mine"); err != nil {
		t.Errorf("DeleteView: %v", err)
	}
	if len(store.Board().Views) != 0 {
		t.Errorf("DeleteView: view still present: %+v", store.Board().Views)
	}
	if err := admin.DeleteView("ghost"); err == nil {
		t.Error("DeleteView unknown should fail")
	}

	// --- labels ---

	a, err := store.Create(native.Issue{Title: "la", Labels: []string{"bug", "p1"}})
	if err != nil {
		t.Fatalf("Create la: %v", err)
	}
	b, err := store.Create(native.Issue{Title: "lb", Labels: []string{"bug"}})
	if err != nil {
		t.Fatalf("Create lb: %v", err)
	}
	// RenameLabel cascades to both bug-carrying issues.
	n, err = admin.RenameLabel("bug", "defect")
	if err != nil || n != 2 {
		t.Errorf("RenameLabel: touched=%d err=%v (want 2, nil)", n, err)
	}
	if got, _ := store.Get(a.ID); !contains(got.Labels, "defect") || contains(got.Labels, "bug") {
		t.Errorf("RenameLabel cascade a: %+v", got.Labels)
	}
	if _, err := admin.RenameLabel("", "x"); !errors.Is(err, native.ErrLabelEmpty) {
		t.Errorf("RenameLabel empty: want ErrLabelEmpty, got %v", err)
	}
	if n, err := admin.RenameLabel("defect", "defect"); err != nil || n != 0 {
		t.Errorf("RenameLabel self no-op: touched=%d err=%v", n, err)
	}
	// MergeLabels: merge p1 into defect on issue a (dedupe — a already has defect).
	n, err = admin.MergeLabels("p1", "defect")
	if err != nil || n != 1 {
		t.Errorf("MergeLabels: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(a.ID); contains(got.Labels, "p1") || labelCount(got.Labels, "defect") != 1 {
		t.Errorf("MergeLabels dedupe a: %+v", got.Labels)
	}
	// DeleteLabel strips defect from both.
	n, err = admin.DeleteLabel("defect")
	if err != nil || n != 2 {
		t.Errorf("DeleteLabel: touched=%d err=%v (want 2, nil)", n, err)
	}
	if got, _ := store.Get(b.ID); contains(got.Labels, "defect") {
		t.Errorf("DeleteLabel cascade b: %+v", got.Labels)
	}
	if _, err := admin.DeleteLabel(""); !errors.Is(err, native.ErrLabelEmpty) {
		t.Errorf("DeleteLabel empty: want ErrLabelEmpty, got %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func labelCount(ss []string, want string) int {
	c := 0
	for _, s := range ss {
		if s == want {
			c++
		}
	}
	return c
}

// TestNativeStore_Conformance proves the suite against the reference
// filesystem implementation (always runs).
func TestNativeStore_Conformance(t *testing.T) {
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	runBoardStoreSuite(t, store)

	admin, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore (admin): %v", err)
	}
	runBoardAdminSuite(t, admin, admin)
}

// TestMongoStore_Conformance runs the same suite against the Mongo store.
func TestMongoStore_Conformance(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// Idempotent re-run.
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema (second): %v", err)
	}
	runBoardStoreSuite(t, boardmongo.New(db, "tenant-1"))

	// The same Mongo store must satisfy native.BoardAdmin identically to the
	// filesystem store (its own tenant so the cascades don't collide with the
	// BoardStore suite's tenant-1 issues).
	adminStore := boardmongo.New(db, "admin-tenant")
	runBoardAdminSuite(t, adminStore, adminStore)

	// The Mongo store must also drive the dispatcher as a tracker.Tracker via
	// the shared native.Adapter (eligible + unclaimed + blocker-free filtering).
	runTrackerSuite(t, boardmongo.New(db, "tracker-tenant"))

	// The Coordinator's cross-tenant ListEligible must find ready+unclaimed
	// cards across tenants (verifies the issue.state / issue.claim BSON paths).
	coord := boardmongo.NewCoordinator(db)
	for _, tc := range []struct {
		tenant, title, state string
		claim                bool
	}{
		{"ca", "ready-a", native.StateReady, false},
		{"cb", "ready-b", native.StateReady, false},
		{"ca", "parked", native.StateInbox, false}, // not eligible
		{"cb", "claimed", native.StateReady, true}, // eligible state but claimed
	} {
		st := coord.StoreFor(tc.tenant)
		iss, cerr := st.Create(native.Issue{Title: tc.title, State: tc.state})
		if cerr != nil {
			t.Fatalf("coord create %s: %v", tc.title, cerr)
		}
		if tc.claim {
			if _, cerr := st.Claim(iss.ID, "someone"); cerr != nil {
				t.Fatalf("claim: %v", cerr)
			}
		}
	}
	elig, eerr := coord.ListEligible(ctx, []string{native.StateReady}, 50, false)
	if eerr != nil {
		t.Fatalf("ListEligible: %v", eerr)
	}
	// The Coordinator is cross-tenant BY DESIGN, and this suite shares one
	// database: ready+unclaimed residue from the earlier per-tenant suites
	// (tenant-1's cards) legitimately shows up here. Scope the assertions
	// to this section's own tenants — a real attribution bug still fails
	// on the per-title tenant checks below.
	coordTenants := map[string]bool{"ca": true, "cb": true}
	filtered := elig[:0:0]
	for _, c := range elig {
		if coordTenants[c.Tenant] {
			filtered = append(filtered, c)
		}
	}
	elig = filtered
	gotTitles := map[string]string{}
	for _, c := range elig {
		gotTitles[c.Issue.Title] = c.Tenant
	}
	if gotTitles["ready-a"] != "ca" || gotTitles["ready-b"] != "cb" {
		t.Errorf("cross-tenant ListEligible should return ready-a + ready-b: %v", gotTitles)
	}
	if _, ok := gotTitles["parked"]; ok {
		t.Error("inbox card must not be eligible")
	}
	if _, ok := gotTitles["claimed"]; ok {
		t.Error("claimed card must not be eligible")
	}

	// Ordering contract: the dispatch tick lists oldest-updated first (FIFO
	// fairness); the stranded-card sweep lists NEWEST first, so a capped
	// window always contains the freshest strandings on a saturated board
	// (R0544a9). ready-a was created before ready-b, so their UpdatedAt
	// order is creation order. The count is FATAL: a stray ready+unclaimed
	// card leaked by an earlier suite would otherwise silently void the
	// ordering assertions below.
	if len(elig) != 2 {
		t.Fatalf("cross-tenant eligible count = %d (%v), want exactly ready-a + ready-b", len(elig), gotTitles)
	}
	if elig[0].Issue.Title != "ready-a" || elig[1].Issue.Title != "ready-b" {
		t.Errorf("oldest-first order = [%s, %s], want [ready-a, ready-b]", elig[0].Issue.Title, elig[1].Issue.Title)
	}
	desc, derr := coord.ListEligible(ctx, []string{native.StateReady}, 50, true)
	if derr != nil {
		t.Fatalf("ListEligible newest-first: %v", derr)
	}
	var descOwn []boardmongo.Candidate
	for _, c := range desc {
		if coordTenants[c.Tenant] {
			descOwn = append(descOwn, c)
		}
	}
	if len(descOwn) == 0 || descOwn[0].Issue.Title != "ready-b" {
		t.Errorf("newest-first order = %v, want the freshest card ready-b first", descOwn)
	}
}

// runTrackerSuite exercises the tracker.Tracker view (native.Adapter) over a
// board store — the path the cloud dispatcher uses.
func runTrackerSuite(t *testing.T, store native.BoardStore) {
	t.Helper()
	trk := native.NewAdapter(store)
	ctx := context.Background()

	// An inbox issue is NOT a candidate (inbox is not eligible); a ready issue
	// IS (ready is eligible on the default board).
	_, _ = store.Create(native.Issue{Title: "parked", State: native.StateInbox})
	ready, err := store.Create(native.Issue{Title: "do me", State: native.StateReady})
	if err != nil {
		t.Fatalf("create ready: %v", err)
	}
	cands, err := trk.ListCandidates(ctx)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != ready.ID {
		t.Fatalf("candidates: want [%s], got %+v", ready.ID, cands)
	}

	// Claim removes it from the candidate set.
	if err := trk.Claim(ctx, ready.ID, "runner-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	cands, _ = trk.ListCandidates(ctx)
	if len(cands) != 0 {
		t.Errorf("claimed issue must not be a candidate, got %+v", cands)
	}

	// UpdateState + RefreshStates round-trip.
	if err := trk.UpdateState(ctx, ready.ID, native.StateDone); err != nil {
		t.Errorf("UpdateState: %v", err)
	}
	states, _ := trk.RefreshStates(ctx, []string{ready.ID})
	if states[ready.ID] != native.StateDone {
		t.Errorf("RefreshStates: %v", states)
	}
}

// TestCoordinatorServerNow: the reaper measures server-stamped leases, so
// its cutoff must come from the server too. A coordinator that silently
// returned the zero time (or the client's clock) would put the skew hole
// back exactly where $$NOW closed it.
func TestCoordinatorServerNow(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_clock_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	coord := boardmongo.NewCoordinator(db)

	// Empty collection: no document to project from — the documented
	// zero return, which the caller reads as "fall back to my clock".
	if got, err := coord.ServerNow(ctx); err != nil || !got.IsZero() {
		t.Fatalf("ServerNow on an empty board = (%v, %v), want the zero time and no error", got, err)
	}

	st := boardmongo.New(db, "tenant-clock")
	if _, err := st.Create(native.Issue{Title: "clock probe"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := coord.ServerNow(ctx)
	if err != nil {
		t.Fatalf("ServerNow: %v", err)
	}
	if got.IsZero() {
		t.Fatal("ServerNow returned the zero time with a document present — the reaper would fall back to the pod clock forever")
	}
	if delta := time.Since(got); delta > time.Minute || delta < -time.Minute {
		t.Fatalf("ServerNow = %s, more than a minute from this host's clock (%s) — not a plausible server instant", got, delta)
	}
}

// TestCoordinatorSeesTheUnleasedArm: the CLOUD reaper lists through the
// Coordinator, not the tenant-scoped Store. When only the Store learned
// the un-leased recovery arm, the recovery path the strict fence cites as
// its justification was dead on the twin that has no boot sweep — a card
// whose lease field an older binary dropped stayed held by a dead pod for
// ever, and nothing but a database edit could free it.
func TestCoordinatorSeesTheUnleasedArm(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_coordarm_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-coordarm")
	coord := boardmongo.NewCoordinator(db)

	ghost, err := st.Create(native.Issue{Title: "lease dropped by an old binary", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	issues := db.Collection(boardmongo.IssuesCollection)
	if _, err := issues.UpdateOne(ctx, bson.M{"_id": ghost.ID}, bson.M{
		"$set":   bson.M{"issue.claim": "dead-pod", "issue.updatedat": time.Now().Add(-72 * time.Hour)},
		"$unset": bson.M{"issue.claimleaseuntil": "", "issue.claimepoch": ""},
	}); err != nil {
		t.Fatalf("seed the ghost: %v", err)
	}

	cands, err := coord.ListExpiredClaimCandidates(ctx, time.Now(), 50)
	if err != nil {
		t.Fatalf("ListExpiredClaimCandidates (cross-tenant): %v", err)
	}
	var found *tracker.ExpiredClaim
	for i := range cands {
		if cands[i].Claim.IssueID == ghost.ID {
			found = &cands[i].Claim
		}
	}
	if found == nil {
		t.Fatal("the cloud reaper's own listing must reach a claim carrying no lease — otherwise the card is " +
			"held by a dead pod for ever, and cloud has no boot sweep to free it")
	}
	// And what it lists, the transfer must accept.
	if _, _, err := coord.ReclaimExpired(ctx, "tenant-coordarm", ghost.ID, found.Prev, "reaper:probe", time.Now()); err != nil {
		t.Fatalf("the transfer must accept a candidate the cross-tenant listing produced, got %v", err)
	}
}
