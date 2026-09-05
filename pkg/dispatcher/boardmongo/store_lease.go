package boardmongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// The fenced (owner-scoped) write family: every mutation here is a CAS
// on (issue.claim, issue.claimepoch), so a worker whose claim was stolen
// finds its late writes refused with tracker.ErrClaimConflict instead of
// clobbering the new owner's state — the ADR-094 effect-outbox contract
// applied to board cards. The FS twin's fence lives in
// native/store_lifecycle.go; the shared conformance suite holds both to
// the same behaviour.

// ownedFilter is the fencing CAS filter every owner-scoped write uses.
func (s *Store) ownedFilter(id string, tok tracker.ClaimToken) bson.M {
	f := bson.M{"_id": id, "tenant_id": s.tenant, "issue.claim": tok.Marker}
	if tok.Epoch == 0 {
		// A zero epoch can only come from a claim written before the fence
		// existed; match its absent-or-zero form so the filter never
		// silently matches nothing. Safe because Claim always bumps: no
		// token minted by this code carries 0.
		f["issue.claimepoch"] = bson.M{"$in": bson.A{int64(0), nil}}
	} else {
		// STRICT otherwise — including when the document has no epoch at
		// all. Admitting an absent epoch looks harmless (the marker still
		// pins ownership) and is not: a marker can have SUCCESSIVE
		// generations (release then re-claim by the same worker), and with
		// the field gone every generation matches. Measured: a superseded
		// token re-stamped the fence at its own older value and locked the
		// live holder out of its own card. A holder refused here stops
		// cleanly, which is the safe failure; the card is recovered by
		// ListExpiredClaimCandidates' un-leased arm below.
		f["issue.claimepoch"] = tok.Epoch
	}
	return f
}

// ownedRefused turns a zero-match CAS into the right typed error: the
// card is gone, or the claim moved on (stolen / released / re-acquired).
func (s *Store) ownedRefused(ctx context.Context, id string, tok tracker.ClaimToken) error {
	iss, gerr := s.get(ctx, id)
	if gerr != nil {
		return gerr
	}
	return fmt.Errorf("%w: issue now held by %q (epoch %d, token epoch %d)",
		tracker.ErrClaimConflict, iss.Claim, iss.ClaimEpoch, tok.Epoch)
}

// RenewClaim pushes the lease forward under the token — the heartbeat.
// No event and no UpdatedAt bump: a renewal is liveness, not content,
// and stamping either would churn board_events and the sweeps' ordering
// on every heartbeat of every claimed card.
func (s *Store) RenewClaim(id string, tok tracker.ClaimToken) error {
	return s.RenewClaimCtx(context.Background(), id, tok)
}

// RenewClaimCtx is RenewClaim bounded by the CALLER's context on top of
// the store's own op timeout. The claim session cancels its renewal
// context when Stop() runs — a cancel that reached no store made the
// documented "Stop is not hostage to a slow renewal" guarantee false in
// production: Stop runs on the dispatcher ACTOR (and inside the cloud
// drain's WaitGroup), so a slow Mongo held the whole dispatcher for the
// full op timeout.
func (s *Store) RenewClaimCtx(parent context.Context, id string, tok tracker.ClaimToken) error {
	ctx, cancel := context.WithTimeout(parent, opTimeout)
	defer cancel()
	res, err := s.issues.UpdateOne(ctx, s.ownedFilter(id, tok), leaseStampPipeline(bson.M{}))
	if err != nil {
		return fmt.Errorf("boardmongo: renew claim: %w", err)
	}
	if res.MatchedCount == 0 {
		return s.ownedRefused(ctx, id, tok)
	}
	return nil
}

// ReleaseOwned releases under the token. Idempotent when the claim is
// already gone; a claim held by another marker or epoch refuses —
// releasing someone else's claim is exactly what the fence forbids.
func (s *Store) ReleaseOwned(id string, tok tracker.ClaimToken) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	res, err := s.issues.UpdateOne(ctx, s.ownedFilter(id, tok), bson.M{"$set": bson.M{
		"issue.claim":           "",
		"issue.claimedat":       time.Time{},
		"issue.claimleaseuntil": time.Time{},
		"issue.updatedat":       time.Now().UTC(),
	}})
	if err != nil {
		return fmt.Errorf("boardmongo: release owned: %w", err)
	}
	if res.MatchedCount == 0 {
		if iss, gerr := s.get(ctx, id); gerr == nil && iss.Claim == "" {
			return nil // already released — the desired state
		}
		return s.ownedRefused(ctx, id, tok)
	}
	return s.emit(native.Event{Type: native.EvtIssueReleased, IssueID: id,
		Payload: map[string]any{"marker": tok.Marker}})
}

// stateSet builds the $set every state writer here shares. The give-up
// stamp goes with it: the FS twin expires the buffer on EVERY write
// (writeIssueLocked → expireGiveUp), and the targeted $set writers this
// package added would otherwise leave a card reading "the dispatcher gave
// up" after something moved it on. The stamp only describes the state it
// was taken in — a card that left that state has no give-up to show.
func stateSet(newState string) bson.M { return stateSetAt(newState, time.Now().UTC()) }

// stateSetAt is stateSet with the write's timestamp supplied, so a caller
// that must ALSO return the updated issue can report exactly what it
// wrote instead of a pre-write snapshot.
func stateSetAt(newState string, at time.Time) bson.M {
	return bson.M{
		"issue.state":     newState,
		"issue.stateat":   at,
		"issue.gaveup":    nil,
		"issue.updatedat": at,
	}
}

// SetStateOwned is SetState fenced on the claim token: the ownership
// check and the transition are ONE conditional write.
func (s *Store) SetStateOwned(id, newState string, tok tracker.ClaimToken) (*native.Issue, error) {
	return s.setStateOwnedReason(id, newState, tok, "")
}

// SetStateOwnedReason is SetStateOwned with an EXPLICIT provenance
// overriding the marker-derived one (native.SetStateOwnedReason's twin):
// the watchdog's terminal filings carry the run's own verdict so the
// card's downstream chain fires as it would have for the living owner.
func (s *Store) SetStateOwnedReason(id, newState string, tok tracker.ClaimToken, reason string) (*native.Issue, error) {
	return s.setStateOwnedReason(id, newState, tok, reason)
}

func (s *Store) setStateOwnedReason(id, newState string, tok tracker.ClaimToken, reason string) (*native.Issue, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	dst := board.StateByName(newState)
	if dst == nil {
		return nil, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, newState)
	}
	// Holding the claim is not a licence to resurrect a card an operator
	// closed: the terminal sink binds the OWNED family too. The guard
	// rides INTO the CAS as a `$nin` on the source state (a check-then-act
	// would reopen the TOCTOU the fence exists to close); terminal→terminal
	// stays free, it is an operator refiling — see native.ValidateStateExit.
	// Same-state is a no-op on the FS twin (setStateLocked returns early),
	// so it must be one here: writing anyway churns UpdatedAt — which the
	// newest-first sweeps and the board_events tail order on — and
	// stateSet would clear a give-up stamp that still describes the state
	// the card is actually in.
	if cur, gerr := s.get(ctx, id); gerr == nil && cur.State == newState {
		n, cerr := s.issues.CountDocuments(ctx, s.ownedFilter(id, tok))
		if cerr != nil {
			return nil, fmt.Errorf("boardmongo: verify claim ownership: %w", cerr)
		}
		if n == 0 {
			return nil, s.ownedRefused(ctx, id, tok)
		}
		return cur, nil
	}
	filter := s.ownedFilter(id, tok)
	if !dst.Terminal {
		if sinks := native.TerminalStateNames(board); len(sinks) > 0 {
			filter["issue.state"] = bson.M{"$nin": sinks}
		}
	}
	// Before, not After: the event payload needs the state being LEFT, and
	// the retry below re-reads it the same way. writtenAt lets the return
	// value below still describe what was WRITTEN rather than the
	// pre-write snapshot.
	writtenAt := time.Now().UTC()
	res := s.issues.FindOneAndUpdate(ctx, filter,
		bson.M{"$set": stateSetAt(newState, writtenAt)},
		options.FindOneAndUpdate().SetReturnDocument(options.Before))
	if res.Err() != nil && isNoDocuments(res.Err()) {
		// Zero match has three causes and they must not be conflated: the
		// claim moved on, the card sits in a sink, or the card simply
		// changed under us between the CAS and now. Qualify ownership
		// FIRST (the FS twin's order) — a stolen claim reported as a
		// transition rejection is swallowed by the live finish worker as
		// an Info line, losing the one event this fence exists to surface.
		n, cerr := s.issues.CountDocuments(ctx, s.ownedFilter(id, tok))
		if cerr != nil {
			return nil, fmt.Errorf("boardmongo: verify claim ownership: %w", cerr)
		}
		if n == 0 {
			return nil, s.ownedRefused(ctx, id, tok)
		}
		if iss, gerr := s.get(ctx, id); gerr == nil {
			if verr := native.ValidateStateExit(board, iss.State, newState); verr != nil {
				return nil, verr
			}
		}
		// We still own the claim and the sink does not refuse us: the card
		// moved in the window. Retry ONCE rather than synthesise a claim
		// conflict the counter just disproved — the caller's session
		// treats ErrClaimConflict as terminal and would abandon a card it
		// still holds.
		writtenAt = time.Now().UTC()
		res = s.issues.FindOneAndUpdate(ctx, filter,
			bson.M{"$set": stateSetAt(newState, writtenAt)},
			options.FindOneAndUpdate().SetReturnDocument(options.Before))
		if res.Err() != nil && isNoDocuments(res.Err()) {
			// Still nothing, with ownership just verified: the card is
			// moving under us repeatedly. Say THAT — reporting the claim
			// conflict the counter disproved would make the caller's
			// session latch `lost` and abandon a card it still holds.
			return nil, fmt.Errorf("%w: %s moved twice while setting state %q — retry",
				tracker.ErrTransitionRejected, id, newState)
		}
	}
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return nil, fmt.Errorf("boardmongo: set state owned: %w", res.Err())
		}
		return nil, s.ownedRefused(ctx, id, tok)
	}
	var before issueDoc
	if err := res.Decode(&before); err != nil {
		return nil, fmt.Errorf("boardmongo: set state owned decode: %w", err)
	}
	updated := before.Issue
	old := updated.State
	// Mirror EVERY field the write touched, not just the state: stateSet
	// also clears the give-up stamp and bumps updatedat, and a returned
	// value that still carries the old ones describes a card that no
	// longer exists.
	updated.State = newState
	updated.GaveUp = nil
	updated.UpdatedAt = writtenAt
	if old != newState {
		if err := s.emit(native.Event{Type: native.EvtIssueState, IssueID: id,
			Payload: func() map[string]any {
				if reason != "" {
					return map[string]any{"from": old, "to": newState, "reason": reason}
				}
				return native.StateChangePayload(old, newState, tok.Marker)
			}()}); err != nil {
			return nil, err
		}
		if newState == native.StateDone {
			// Best-effort: do not roll back a successful done transition.
			_ = native.PromoteUnblockedDependents(s, id)
		}
	}
	return &updated, nil
}

// SetStateOwnedFrom is SetStateOwned with a source-state precondition
// (see native.BoardStore): the fencing filter and `issue.state == from`
// ride in ONE FindOneAndUpdate. On a zero match ownership is qualified
// FIRST (the FS twin's order) — a stolen claim reported as "drifted"
// would be swallowed by the caller as an ordinary operator move.
func (s *Store) SetStateOwnedFrom(id, from, to string, tok tracker.ClaimToken) (*native.Issue, bool, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	if board.StateByName(to) == nil {
		return nil, false, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, to)
	}
	if from != to {
		if err := native.ValidateStateExit(board, from, to); err != nil {
			return nil, false, err
		}
	}
	if from == to {
		// Nothing to perform, so nothing is CHANGED (the FS twin's answer)
		// — but an idempotent no-op must not mask a stolen claim from a
		// caller about to keep writing.
		n, cerr := s.issues.CountDocuments(ctx, s.ownedFilter(id, tok))
		if cerr != nil {
			return nil, false, fmt.Errorf("boardmongo: verify claim ownership: %w", cerr)
		}
		if n == 0 {
			return nil, false, s.ownedRefused(ctx, id, tok)
		}
		iss, err := s.get(ctx, id)
		return iss, false, err
	}
	filter := s.ownedFilter(id, tok)
	filter["issue.state"] = from
	writtenAt := time.Now().UTC()
	res := s.issues.FindOneAndUpdate(ctx, filter,
		bson.M{"$set": stateSetAt(to, writtenAt)},
		options.FindOneAndUpdate().SetReturnDocument(options.After))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return nil, false, fmt.Errorf("boardmongo: set state owned from: %w", res.Err())
		}
		n, cerr := s.issues.CountDocuments(ctx, s.ownedFilter(id, tok))
		if cerr != nil {
			return nil, false, fmt.Errorf("boardmongo: verify claim ownership: %w", cerr)
		}
		if n == 0 {
			return nil, false, s.ownedRefused(ctx, id, tok)
		}
		iss, gerr := s.get(ctx, id)
		if gerr != nil {
			return nil, false, gerr
		}
		return iss, false, nil
	}
	var doc issueDoc
	if err := res.Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("boardmongo: set state owned from decode: %w", err)
	}
	if err := s.emit(native.Event{Type: native.EvtIssueState, IssueID: id,
		Payload: native.StateChangePayload(from, to, tok.Marker)}); err != nil {
		return nil, false, err
	}
	if to == native.StateDone {
		// Best-effort: do not roll back a successful done transition.
		_ = native.PromoteUnblockedDependents(s, id)
	}
	return &doc.Issue, true, nil
}

// SetLastRunOwned is SetLastRun fenced on the claim token. The run-ref
// history merge needs the current value, so this is read-then-CAS: the
// write still only lands while the token owns the claim.
func (s *Store) SetLastRunOwned(id, runID, workdir string, tok tracker.ClaimToken) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if iss.LastRunID == runID && iss.LastWorkdir == workdir {
		// Still verify ownership: an idempotent no-op must not mask a
		// stolen claim from a caller about to keep writing. Probe with
		// ownedFilter rather than comparing the snapshot by hand — one
		// definition of "does this token own this card", so the healed
		// absent-epoch document is judged the same way here as everywhere.
		n, cerr := s.issues.CountDocuments(ctx, s.ownedFilter(id, tok))
		if cerr != nil {
			return fmt.Errorf("boardmongo: verify claim ownership: %w", cerr)
		}
		if n == 0 {
			return s.ownedRefused(ctx, id, tok)
		}
		return nil
	}
	now := time.Now().UTC()
	runs := native.AppendRunRef(iss.Runs, runID, workdir, now)
	res, err := s.issues.UpdateOne(ctx, s.ownedFilter(id, tok), bson.M{"$set": bson.M{
		"issue.lastrunid":   runID,
		"issue.lastworkdir": workdir,
		"issue.runs":        runs,
		"issue.updatedat":   now,
	}})
	if err != nil {
		return fmt.Errorf("boardmongo: set last run owned: %w", err)
	}
	if res.MatchedCount == 0 {
		return s.ownedRefused(ctx, id, tok)
	}
	return s.emit(native.Event{Type: native.EvtIssueLastRun, IssueID: id,
		Payload: map[string]any{"run_id": runID, "workdir": workdir}})
}

// SetAwaitingInputOwned is SetAwaitingInput fenced on the claim token.
func (s *Store) SetAwaitingInputOwned(id string, v bool, tok tracker.ClaimToken) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	res := s.issues.FindOneAndUpdate(ctx, s.ownedFilter(id, tok),
		bson.M{"$set": bson.M{"issue.awaitinginput": v, "issue.updatedat": time.Now().UTC()}},
		options.FindOneAndUpdate().SetReturnDocument(options.Before))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return fmt.Errorf("boardmongo: set awaiting input owned: %w", res.Err())
		}
		return s.ownedRefused(ctx, id, tok)
	}
	var before issueDoc
	if err := res.Decode(&before); err != nil {
		return fmt.Errorf("boardmongo: set awaiting input owned decode: %w", err)
	}
	if before.Issue.AwaitingInput == v {
		return nil
	}
	return s.emit(native.Event{Type: native.EvtIssueUpdated, IssueID: id,
		Payload: map[string]any{"awaiting_input": v}})
}

// SetGaveUpOwned is SetGaveUp fenced on the claim token. The stamp's
// supersession rules need the current issue, so read-then-CAS like
// SetLastRunOwned.
func (s *Store) SetGaveUpOwned(id string, g *native.GiveUp, tok tracker.ClaimToken) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	// Ownership through the ONE helper, never a hand-rolled comparison: a
	// recopied guard drifts, and this one did — its strict epoch equality
	// refused exactly the healed-absent-epoch document ownedFilter admits,
	// losing the give-up stamp during a rolling deploy, which is when a
	// give-up matters most.
	n, cerr := s.issues.CountDocuments(ctx, s.ownedFilter(id, tok))
	if cerr != nil {
		return fmt.Errorf("boardmongo: verify claim ownership: %w", cerr)
	}
	if n == 0 {
		return s.ownedRefused(ctx, id, tok)
	}
	want, ok := native.GiveUpToRecord(iss, g)
	if !ok || native.SameGiveUp(iss.GaveUp, want) {
		return nil
	}
	var stamped *native.GiveUp
	if want != nil {
		stamp := *want
		if stamp.At.IsZero() {
			stamp.At = time.Now().UTC()
		}
		stamped = &stamp
	}
	res, err := s.issues.UpdateOne(ctx, s.ownedFilter(id, tok), bson.M{"$set": bson.M{
		"issue.gaveup":    stamped,
		"issue.updatedat": time.Now().UTC(),
	}})
	if err != nil {
		return fmt.Errorf("boardmongo: set gave up owned: %w", err)
	}
	if res.MatchedCount == 0 {
		return s.ownedRefused(ctx, id, tok)
	}
	payload := map[string]any{"gave_up": stamped != nil}
	if stamped != nil {
		payload["run_id"] = stamped.RunID
		payload["state"] = stamped.State
		payload["attempts"] = stamped.Attempts
		if stamped.Reason != "" {
			payload["reason"] = stamped.Reason
		}
	}
	return s.emit(native.Event{Type: native.EvtIssueGaveUp, IssueID: id, Payload: payload})
}

func isNoDocuments(err error) bool { return errors.Is(err, mongo.ErrNoDocuments) }

// unleasedClaimHorizon is how stale a claim carrying NO lease must be
// before the watchdog will touch it. Deliberately far longer than the
// lease: an expired lease is positive evidence a heartbeat stopped,
// while a missing one is only an absence — so the bar is "nothing has
// touched this card in a very long time" rather than "a beat was
// missed". The default is the trade-off for a mixed fleet: during a
// rolling deploy an OLD binary strips leases as it writes and does not
// heartbeat, so a short horizon would release a card its old-binary
// holder is still working. Tunable per deployment through
// UnleasedClaimHorizonEnv (see SetUnleasedClaimHorizon) — set BEFORE the
// coordinator runs; the queries read it unguarded.
var unleasedClaimHorizon = DefaultUnleasedClaimHorizon

// DefaultUnleasedClaimHorizon is the shipped horizon.
const DefaultUnleasedClaimHorizon = 24 * time.Hour

// UnleasedClaimHorizonEnv overrides the un-leased horizon (a Go duration,
// e.g. "2h"). It must be at least one claim lease: below that, a claim
// with NO lease would be reclaimable sooner than one whose lease merely
// expired, inverting the evidence ordering the arms are built on.
const UnleasedClaimHorizonEnv = "ITERION_BOARD_UNLEASED_CLAIM_HORIZON"

// UnleasedClaimHorizon reports the horizon in force.
func UnleasedClaimHorizon() time.Duration { return unleasedClaimHorizon }

// SetUnleasedClaimHorizon installs the horizon. Refused loudly below one
// lease (see UnleasedClaimHorizonEnv) — never silently clamped.
func SetUnleasedClaimHorizon(d time.Duration) error {
	if d < native.ClaimLeaseDuration {
		return fmt.Errorf("%s: %s is below one claim lease (%s) — an un-leased claim would be reclaimable sooner than an expired one",
			UnleasedClaimHorizonEnv, d, native.ClaimLeaseDuration)
	}
	unleasedClaimHorizon = d
	return nil
}

// ConfigureUnleasedClaimHorizonFromEnv applies UnleasedClaimHorizonEnv
// when set and reports the horizon in force. An unparsable or too-short
// value is an error the caller surfaces at startup: a watchdog measuring
// against a horizon nobody intended is worse than one that refuses to
// start.
func ConfigureUnleasedClaimHorizonFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(UnleasedClaimHorizonEnv))
	if raw == "" {
		return unleasedClaimHorizon, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", UnleasedClaimHorizonEnv, raw, err)
	}
	if err := SetUnleasedClaimHorizon(d); err != nil {
		return 0, err
	}
	return d, nil
}

// reclaimableLease is the ONE definition of "this claim may be taken":
// an expired lease, or none at all past a much longer horizon. The
// listing and the transfer's CAS both build from it — when they drifted
// apart, the listing produced candidates the transfer could never accept,
// so every pass listed and refused the same cards for ever.
//
// The two arms are not equivalent evidence. An expired lease is positive:
// a heartbeat stopped. A missing one is an absence — an older binary's
// full-document replace, or a claim predating the lease entirely — so it
// only qualifies once nothing has touched the card in a very long time.
func reclaimableLease(cutoff time.Time) []bson.M {
	return []bson.M{
		{"issue.claimleaseuntil": bson.M{"$gt": time.Time{}, "$lt": cutoff}},
		UnleasedArm(cutoff),
	}
}

// UnleasedArm is the second arm on its own: a claim carrying NO lease,
// untouched for a long time. It is exported because it names a distinct
// POPULATION, not just half a query — the cards a mixed-fleet write
// stripped of their lease. Their holder can no longer renew, write, or
// even release, so nothing recovers them except a sweep that goes
// looking; and since a rolling deploy is what creates them, that sweep
// cannot be the one behind the reaper gate, which ships off.
func UnleasedArm(cutoff time.Time) bson.M {
	return bson.M{
		"issue.claimleaseuntil": bson.M{"$in": bson.A{time.Time{}, nil}},
		"issue.updatedat":       bson.M{"$lt": cutoff.Add(-unleasedClaimHorizon)},
	}
}

// ListExpiredClaimCandidates — see tracker.ClaimReaper (Mongo twin).
// The two arms are queried SEPARATELY, expired leases first: they sort
// by lease instant, and a missing lease sorts before every real one, so
// a single query would let un-leased stragglers fill the batch and starve
// the cards the watchdog exists to act on.
func (s *Store) ListExpiredClaimCandidates(cutoff time.Time, limit int) ([]tracker.ExpiredClaim, error) {
	if limit <= 0 {
		limit = 100
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	arms := reclaimableLease(cutoff)
	out := make([]tracker.ExpiredClaim, 0, limit)
	for _, arm := range arms {
		if len(out) >= limit {
			break
		}
		filter := bson.M{"tenant_id": s.tenant, "issue.claim": bson.M{"$gt": ""}}
		for k, v := range arm {
			filter[k] = v
		}
		cur, err := s.issues.Find(ctx, filter,
			options.Find().SetSort(bson.D{{Key: "issue.claimleaseuntil", Value: 1}}).
				SetLimit(int64(limit-len(out))))
		if err != nil {
			return nil, fmt.Errorf("boardmongo: list expired claims: %w", err)
		}
		var docs []issueDoc
		if err := cur.All(ctx, &docs); err != nil {
			return nil, fmt.Errorf("boardmongo: decode expired claims: %w", err)
		}
		for _, d := range docs {
			iss := d.Issue
			out = append(out, native.ExpiredClaimFrom(&iss))
		}
	}
	return out, nil
}

// ReclaimExpired — see tracker.ClaimReaper (Mongo twin): one CAS
// carrying the whole precondition (claim still exactly prev, lease
// still expired), the epoch bump, and the fresh recovery lease stamped
// with the SERVER clock.
func (s *Store) ReclaimExpired(id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tracker.ClaimToken, string, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	// The fencing precondition is exactly ownedFilter's (claim == prev,
	// legacy-epoch handling included — one definition of "how a
	// zero epoch matches") plus the still-expired lease bound.
	filter := s.ownedFilter(id, prev)
	// Mirror the listing exactly (reclaimableLease): a candidate the
	// listing produced must be one this CAS can accept, or the watchdog
	// re-lists and re-refuses the same card on every pass.
	filter["$or"] = reclaimableLease(cutoff)
	res := s.issues.FindOneAndUpdate(ctx, filter,
		leaseStampPipeline(bson.M{
			// $literal: pipeline $set evaluates values as EXPRESSIONS, and
			// the marker is operator-configurable — a "$"-leading one
			// would be read as a field path (the AddComment lesson,
			// applied to its class sibling).
			"issue.claim":      bson.M{"$literal": marker},
			"issue.claimepoch": bumpEpochExpr(),
			"issue.claimedat":  "$$NOW",
			"issue.updatedat":  "$$NOW",
		}),
		options.FindOneAndUpdate().SetReturnDocument(options.After))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return tracker.ClaimToken{}, "", fmt.Errorf("boardmongo: reclaim expired: %w", res.Err())
		}
		iss, gerr := s.get(ctx, id)
		if gerr != nil {
			return tracker.ClaimToken{}, "", gerr
		}
		return tracker.ClaimToken{}, "", fmt.Errorf("%w: claim moved on (now %q epoch %d)",
			tracker.ErrClaimConflict, iss.Claim, iss.ClaimEpoch)
	}
	var doc issueDoc
	if err := res.Decode(&doc); err != nil {
		return tracker.ClaimToken{}, "", fmt.Errorf("boardmongo: reclaim decode: %w", err)
	}
	if err := s.emit(native.Event{Type: native.EvtIssueClaimed, IssueID: id,
		Payload: map[string]any{"marker": marker, "claim_epoch": doc.Issue.ClaimEpoch, "reclaimed_from": prev.Marker}}); err != nil {
		return tracker.ClaimToken{}, "", err
	}
	return tracker.ClaimToken{Marker: marker, Epoch: doc.Issue.ClaimEpoch}, doc.Issue.State, nil
}

// Reopen is the ONE sanctioned exit from a terminal state (see
// native/state_guard.go): terminal-only source, working-state target,
// refused when dependents were already promoted on this card's DONE.
func (s *Store) Reopen(id, toState string) (*native.Issue, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	iss, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	board := s.Board()
	st := board.StateByName(iss.State)
	if st == nil || !st.Terminal {
		return nil, fmt.Errorf("%w: %q is not terminal — use an ordinary state move", tracker.ErrTransitionRejected, iss.State)
	}
	to := board.StateByName(toState)
	if to == nil {
		return nil, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, toState)
	}
	if to.Terminal && toState != iss.State {
		return nil, fmt.Errorf("%w: reopen targets a working state, not another terminal (%q)", tracker.ErrTransitionRejected, toState)
	}
	all, err := s.List(native.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("boardmongo: reopen dependents: %w", err)
	}
	if err := native.ReopenBlockedByDependents(all, id, iss.State); err != nil {
		return nil, err
	}
	// CAS on the source state: an operator (or another replica's reopen)
	// racing this one loses cleanly instead of double-applying.
	// Return the POST-write document. stateSet also clears the give-up
	// stamp and bumps updatedat, and this value is JSON-encoded straight
	// back to the caller (SetStateOrReopen, via the board HTTP handlers) —
	// so returning the snapshot read above told the studio the reopened
	// card still carried the give-up flag that "Needs attention" reads.
	res := s.issues.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "tenant_id": s.tenant, "issue.state": iss.State},
		bson.M{"$set": stateSet(toState)},
		options.FindOneAndUpdate().SetReturnDocument(options.After))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return nil, fmt.Errorf("boardmongo: reopen: %w", res.Err())
		}
		return nil, fmt.Errorf("%w: card moved while reopening", tracker.ErrTransitionRejected)
	}
	var doc issueDoc
	if err := res.Decode(&doc); err != nil {
		return nil, fmt.Errorf("boardmongo: reopen decode: %w", err)
	}
	old := iss.State
	if err := s.emit(native.Event{Type: native.EvtIssueState, IssueID: id,
		Payload: map[string]any{"from": old, "to": toState, "reopened": true}}); err != nil {
		return nil, err
	}
	return &doc.Issue, nil
}

// SetStateFrom is the CAS move for automated writers — changed=false
// when the state drifted; the terminal guard still applies.
func (s *Store) SetStateFrom(id, from, to string) (*native.Issue, bool, error) {
	return s.SetStateFromReason(id, from, to, "")
}

// SetStateFromReason is SetStateFrom carrying an explicit provenance, the
// CAS twin of SetStateWithReason. A machine repair needs BOTH halves at
// once: the CAS so it cannot overwrite the operator who moved the card
// under its decision, and the reason so the spine does not read the
// repair as an operator gesture (spending a one-shot label and signing
// the move with the assignee's name).
func (s *Store) SetStateFromReason(id, from, to, reason string) (*native.Issue, bool, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	board := s.Board()
	if board.StateByName(to) == nil {
		return nil, false, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, to)
	}
	if from == to {
		iss, err := s.get(ctx, id)
		return iss, false, err
	}
	if err := native.ValidateStateExit(board, from, to); err != nil {
		return nil, false, err
	}
	res := s.issues.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "tenant_id": s.tenant, "issue.state": from},
		bson.M{"$set": stateSet(to)},
		options.FindOneAndUpdate().SetReturnDocument(options.After))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return nil, false, fmt.Errorf("boardmongo: set state from: %w", res.Err())
		}
		iss, gerr := s.get(ctx, id)
		if gerr != nil {
			return nil, false, gerr
		}
		return iss, false, nil
	}
	var doc issueDoc
	if err := res.Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("boardmongo: set state from decode: %w", err)
	}
	updated := doc.Issue
	payload := map[string]any{"from": from, "to": to}
	if reason != "" {
		payload["reason"] = reason
	}
	if err := s.emit(native.Event{Type: native.EvtIssueState, IssueID: id, Payload: payload}); err != nil {
		return nil, false, err
	}
	if to == native.StateDone {
		_ = native.PromoteUnblockedDependents(s, id)
	}
	return &updated, true, nil
}
