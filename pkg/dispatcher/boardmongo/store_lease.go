package boardmongo

import (
	"context"
	"errors"
	"fmt"
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
		// A zero epoch can only come from a legacy claim written before
		// the fence existed; match its absent-or-zero form explicitly so
		// the filter never silently matches nothing.
		f["issue.claimepoch"] = bson.M{"$in": bson.A{int64(0), nil}}
	} else {
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
	ctx, cancel := ctxWithTimeout()
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
		iss, gerr := s.get(ctx, id)
		if gerr != nil {
			return gerr
		}
		if iss.Claim == "" {
			return nil // already released — the desired state
		}
		return fmt.Errorf("%w: issue now held by %q (epoch %d, token epoch %d)",
			tracker.ErrClaimConflict, iss.Claim, iss.ClaimEpoch, tok.Epoch)
	}
	return s.emit(native.Event{Type: native.EvtIssueReleased, IssueID: id,
		Payload: map[string]any{"marker": tok.Marker}})
}

// SetStateOwned is SetState fenced on the claim token: the ownership
// check and the transition are ONE conditional write.
func (s *Store) SetStateOwned(id, newState string, tok tracker.ClaimToken) (*native.Issue, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	if s.Board().StateByName(newState) == nil {
		return nil, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, newState)
	}
	res := s.issues.FindOneAndUpdate(ctx, s.ownedFilter(id, tok),
		bson.M{"$set": bson.M{"issue.state": newState, "issue.updatedat": time.Now().UTC()}},
		options.FindOneAndUpdate().SetReturnDocument(options.Before))
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
	updated.State = newState
	if old != newState {
		if err := s.emit(native.Event{Type: native.EvtIssueState, IssueID: id,
			Payload: map[string]any{"from": old, "to": newState}}); err != nil {
			return nil, err
		}
		if newState == native.StateDone {
			// Best-effort: do not roll back a successful done transition.
			_ = native.PromoteUnblockedDependents(s, id)
		}
	}
	return &updated, nil
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
		// stolen claim from a caller about to keep writing.
		if iss.Claim != tok.Marker || iss.ClaimEpoch != tok.Epoch {
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
	if iss.Claim != tok.Marker || iss.ClaimEpoch != tok.Epoch {
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
	}
	return s.emit(native.Event{Type: native.EvtIssueGaveUp, IssueID: id, Payload: payload})
}

func isNoDocuments(err error) bool { return errors.Is(err, mongo.ErrNoDocuments) }

// ListExpiredClaimCandidates — see tracker.ClaimReaper (Mongo twin).
// Legacy claims (zero lease) are excluded by the strictly-positive
// lower bound; missing fields never match a range operator either.
func (s *Store) ListExpiredClaimCandidates(cutoff time.Time, limit int) ([]tracker.ExpiredClaim, error) {
	if limit <= 0 {
		limit = 100
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	cur, err := s.issues.Find(ctx, bson.M{
		"tenant_id":             s.tenant,
		"issue.claim":           bson.M{"$ne": ""},
		"issue.claimleaseuntil": bson.M{"$gt": time.Time{}, "$lt": cutoff},
	}, options.Find().SetSort(bson.D{{Key: "issue.claimleaseuntil", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("boardmongo: list expired claims: %w", err)
	}
	var docs []issueDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("boardmongo: decode expired claims: %w", err)
	}
	out := make([]tracker.ExpiredClaim, 0, len(docs))
	for _, d := range docs {
		out = append(out, tracker.ExpiredClaim{
			IssueID:    d.Issue.ID,
			Identifier: d.Issue.ID,
			State:      d.Issue.State,
			LastRunID:  d.Issue.LastRunID,
			Prev:       tracker.ClaimToken{Marker: d.Issue.Claim, Epoch: d.Issue.ClaimEpoch},
		})
	}
	return out, nil
}

// ReclaimExpired — see tracker.ClaimReaper (Mongo twin): one CAS
// carrying the whole precondition (claim still exactly prev, lease
// still expired), the epoch bump, and the fresh recovery lease stamped
// with the SERVER clock.
func (s *Store) ReclaimExpired(id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tracker.ClaimToken, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	filter := bson.M{
		"_id": id, "tenant_id": s.tenant,
		"issue.claim":           prev.Marker,
		"issue.claimleaseuntil": bson.M{"$gt": time.Time{}, "$lt": cutoff},
	}
	if prev.Epoch == 0 {
		filter["issue.claimepoch"] = bson.M{"$in": bson.A{int64(0), nil}}
	} else {
		filter["issue.claimepoch"] = prev.Epoch
	}
	res := s.issues.FindOneAndUpdate(ctx, filter,
		leaseStampPipeline(bson.M{
			"issue.claim":      marker,
			"issue.claimepoch": bson.M{"$add": bson.A{bson.M{"$ifNull": bson.A{"$issue.claimepoch", 0}}, 1}},
			"issue.claimedat":  "$$NOW",
			"issue.updatedat":  "$$NOW",
		}),
		options.FindOneAndUpdate().SetReturnDocument(options.After))
	if res.Err() != nil {
		if !isNoDocuments(res.Err()) {
			return tracker.ClaimToken{}, fmt.Errorf("boardmongo: reclaim expired: %w", res.Err())
		}
		iss, gerr := s.get(ctx, id)
		if gerr != nil {
			return tracker.ClaimToken{}, gerr
		}
		return tracker.ClaimToken{}, fmt.Errorf("%w: claim moved on (now %q epoch %d)",
			tracker.ErrClaimConflict, iss.Claim, iss.ClaimEpoch)
	}
	var doc issueDoc
	if err := res.Decode(&doc); err != nil {
		return tracker.ClaimToken{}, fmt.Errorf("boardmongo: reclaim decode: %w", err)
	}
	if err := s.emit(native.Event{Type: native.EvtIssueClaimed, IssueID: id,
		Payload: map[string]any{"marker": marker, "claim_epoch": doc.Issue.ClaimEpoch, "reclaimed_from": prev.Marker}}); err != nil {
		return tracker.ClaimToken{}, err
	}
	return tracker.ClaimToken{Marker: marker, Epoch: doc.Issue.ClaimEpoch}, nil
}
