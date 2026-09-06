package native

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/google/uuid"
)

// ClaimLeaseDuration bounds how long a claim protects its holder without
// a RenewClaim heartbeat. Chosen to match the outcome router's
// RouteClaimLease; the reaper (flag-gated) is the only consumer of an
// expiry — a lease running out has no effect until something reclaims.
const ClaimLeaseDuration = 15 * time.Minute

// Claim sets the claim marker, stamps the lease, and returns the
// ownership token every owner-scoped write must present. Returns
// tracker.ErrClaimConflict if the issue is already claimed by a
// different marker. Idempotent for the same marker: the CURRENT token
// comes back (the epoch does not bump — only a fresh acquisition
// advances the fence) and the lease is refreshed, since the owner
// speaking IS the liveness signal.
func (s *Store) Claim(id, marker string) (tok tracker.ClaimToken, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Claim", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return tracker.ClaimToken{}, err
	}
	if iss.Claim != "" && iss.Claim != marker {
		return tracker.ClaimToken{}, fmt.Errorf("%w: held by %s", tracker.ErrClaimConflict, iss.Claim)
	}
	now := time.Now().UTC()
	if iss.Claim == marker {
		iss.ClaimLeaseUntil = now.Add(ClaimLeaseDuration)
		if err := s.writeIssueLocked(iss); err != nil {
			return tracker.ClaimToken{}, err
		}
		s.index[iss.ID] = cloneIssue(iss)
		return tracker.ClaimToken{Marker: marker, Epoch: iss.ClaimEpoch}, nil
	}
	iss.Claim = marker
	iss.ClaimEpoch++
	iss.ClaimedAt = now
	iss.ClaimLeaseUntil = now.Add(ClaimLeaseDuration)
	iss.UpdatedAt = now
	if err := s.writeIssueLocked(iss); err != nil {
		return tracker.ClaimToken{}, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type: EvtIssueClaimed, IssueID: id,
		Payload: map[string]any{"marker": marker, "claim_epoch": iss.ClaimEpoch},
	}); err != nil {
		return tracker.ClaimToken{}, err
	}
	return tracker.ClaimToken{Marker: marker, Epoch: iss.ClaimEpoch}, nil
}

// ownedIssueLocked loads the issue and verifies tok still owns its
// claim — the fencing gate every owner-scoped write goes through. A
// mismatch (stolen, released, re-acquired) is tracker.ErrClaimConflict:
// the caller's ONLY correct move is to stop writing.
func (s *Store) ownedIssueLocked(id string, tok tracker.ClaimToken) (*Issue, error) {
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, err
	}
	if iss.Claim != tok.Marker || iss.ClaimEpoch != tok.Epoch {
		return nil, fmt.Errorf("%w: issue now held by %q (epoch %d, token epoch %d)",
			tracker.ErrClaimConflict, iss.Claim, iss.ClaimEpoch, tok.Epoch)
	}
	return iss, nil
}

// RenewClaim pushes the claim's lease forward under the token — the
// heartbeat that keeps a live worker's claim from being reaped. No
// event and no UpdatedAt bump: a renewal is liveness, not content, and
// stamping either would churn events.jsonl and the sweeps' ordering on
// every heartbeat of every claimed card.
func (s *Store) RenewClaim(id string, tok tracker.ClaimToken) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("RenewClaim", &err)
	iss, err := s.ownedIssueLocked(id, tok)
	if err != nil {
		return err
	}
	iss.ClaimLeaseUntil = time.Now().UTC().Add(ClaimLeaseDuration)
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return nil
}

// SetLastRunOwned is SetLastRun fenced on the claim token. The check
// and the write share ONE critical section — check-then-call would be
// the very TOCTOU the fence exists to close.
func (s *Store) SetLastRunOwned(id, runID, workdir string, tok tracker.ClaimToken) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetLastRunOwned", &err)
	iss, err := s.ownedIssueLocked(id, tok)
	if err != nil {
		return err
	}
	return s.setLastRunLocked(iss, runID, workdir)
}

// SetAwaitingInputOwned is SetAwaitingInput fenced on the claim token.
func (s *Store) SetAwaitingInputOwned(id string, v bool, tok tracker.ClaimToken) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetAwaitingInputOwned", &err)
	iss, err := s.ownedIssueLocked(id, tok)
	if err != nil {
		return err
	}
	return s.setAwaitingInputLocked(iss, v)
}

// SetGaveUpOwned is SetGaveUp fenced on the claim token.
func (s *Store) SetGaveUpOwned(id string, g *GiveUp, tok tracker.ClaimToken) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetGaveUpOwned", &err)
	iss, err := s.ownedIssueLocked(id, tok)
	if err != nil {
		return err
	}
	return s.setGaveUpLocked(iss, g)
}

// SetLaunchRefusalOwned — see BoardStore.
func (s *Store) SetLaunchRefusalOwned(id string, r *LaunchRefusal, tok tracker.ClaimToken) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetLaunchRefusalOwned", &err)
	iss, err := s.ownedIssueLocked(id, tok)
	if err != nil {
		return err
	}
	return s.setLaunchRefusalLocked(iss, r)
}

func (s *Store) setLaunchRefusalLocked(iss *Issue, r *LaunchRefusal) error {
	if r == nil && iss.LaunchRefusal == nil {
		return nil
	}
	iss.LaunchRefusal = r.Clone()
	// UpdatedAt moves on purpose: the dispatch listing is oldest-updated
	// first, so a refused card takes its place behind the cards that have
	// not been tried yet.
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type:    EvtIssueLaunchRefused,
		IssueID: iss.ID,
		Payload: LaunchRefusalPayload(iss.LaunchRefusal),
	})
}

// LaunchRefusalPayload is the EvtIssueLaunchRefused body, shared by both
// twins: {refused: false} for a clear, else the ledger's attempts, next
// instant and reason.
func LaunchRefusalPayload(r *LaunchRefusal) map[string]any {
	if r == nil {
		return map[string]any{"refused": false}
	}
	return map[string]any{
		"refused":    true,
		"attempts":   r.Attempts,
		"not_before": r.NotBefore,
		"reason":     r.LastReason,
	}
}

// ReleaseOwned is Release fenced on the claim token. Idempotent when
// the claim is already gone (an unclaimed issue is the desired state);
// a claim held by ANOTHER epoch or marker refuses — releasing someone
// else's claim is exactly what the fence forbids.
func (s *Store) ReleaseOwned(id string, tok tracker.ClaimToken) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("ReleaseOwned", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	if iss.Claim == "" {
		return nil
	}
	if iss.Claim != tok.Marker || iss.ClaimEpoch != tok.Epoch {
		return fmt.Errorf("%w: issue now held by %q (epoch %d, token epoch %d)",
			tracker.ErrClaimConflict, iss.Claim, iss.ClaimEpoch, tok.Epoch)
	}
	return s.releaseLocked(iss, tok.Marker)
}

// SetLastRun stamps the most recent dispatcher-spawned run that
// processed the issue onto its record. Idempotent — passing the same
// runID + workdir as the current values is a no-op (no write, no
// event). Empty strings are written as-is so the operator can clear
// the stamp if needed.
//
// The dispatcher calls this on every finishRun (success or failure)
// so the studio's IssueModal can always link back to the most recent
// run that touched the issue.
func (s *Store) SetLastRun(id, runID, workdir string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetLastRun", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	return s.setLastRunLocked(iss, runID, workdir)
}

func (s *Store) setLastRunLocked(iss *Issue, runID, workdir string) error {
	if iss.LastRunID == runID && iss.LastWorkdir == workdir {
		return nil
	}
	now := time.Now().UTC()
	iss.LastRunID = runID
	iss.LastWorkdir = workdir
	iss.Runs = AppendRunRef(iss.Runs, runID, workdir, now)
	iss.LaunchRefusal = nil // a launch happened: the retry ledger no longer describes the card
	iss.UpdatedAt = now
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type:    EvtIssueLastRun,
		IssueID: iss.ID,
		Payload: map[string]any{"run_id": runID, "workdir": workdir},
	})
}

// SetAwaitingInput denormalizes onto the issue whether its most recent
// dispatcher-spawned run parked awaiting human/operator input (see
// Issue.AwaitingInput). Idempotent — setting the flag to its current
// value is a no-op (no write, no event). Follows the SetLastRun shape:
// read → set → write → bump UpdatedAt → emit EvtIssueUpdated so tailers
// (studio) refresh the card badge.
func (s *Store) SetAwaitingInput(id string, v bool) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetAwaitingInput", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	return s.setAwaitingInputLocked(iss, v)
}

func (s *Store) setAwaitingInputLocked(iss *Issue, v bool) error {
	if iss.AwaitingInput == v {
		return nil
	}
	iss.AwaitingInput = v
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type:    EvtIssueUpdated,
		IssueID: iss.ID,
		Payload: map[string]any{"awaiting_input": v},
	})
}

// SetGaveUp stamps (or, with a nil g, clears) the dispatcher's give-up on an
// issue — the record that the ticket's current state was written by the
// dispatcher exhausting its retry budget rather than by a human filing it
// (see Issue.GaveUp). Idempotent: re-stamping the same run/state/attempts is
// a no-op, as is clearing an issue that carries no stamp.
//
// Follows the SetAwaitingInput shape (read → set → write → bump UpdatedAt),
// but emits its own EvtIssueGaveUp: a give-up is the one ticket transition
// nobody asked for, and events.jsonl is where an operator reconstructs why a
// ticket ended where it did.
func (s *Store) SetGaveUp(id string, g *GiveUp) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetGaveUp", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	return s.setGaveUpLocked(iss, g)
}

func (s *Store) setGaveUpLocked(iss *Issue, g *GiveUp) error {
	want, ok := GiveUpToRecord(iss, g)
	if !ok {
		return nil
	}
	// Compared against what would ACTUALLY be written, so a repeat call is a
	// real no-op rather than a re-write that churns UpdatedAt.
	if SameGiveUp(iss.GaveUp, want) {
		return nil
	}
	// stamped is what actually landed on the issue (nil for a clear); the
	// event reports IT, never the caller's g, which may name a state the
	// store overrode.
	var stamped *GiveUp
	if want == nil {
		iss.GaveUp = nil
	} else {
		stamp := *want
		if stamp.At.IsZero() {
			stamp.At = time.Now().UTC()
		}
		iss.GaveUp = &stamp
		stamped = &stamp
	}
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	payload := map[string]any{"gave_up": stamped != nil}
	if stamped != nil {
		payload["run_id"] = stamped.RunID
		// The state that was STAMPED, not the one the caller believed — the
		// two differ when a give-up raced an operator move, and the audit
		// record exists to reconstruct what actually happened.
		payload["state"] = stamped.State
		payload["attempts"] = stamped.Attempts
		if stamped.Reason != "" {
			payload["reason"] = stamped.Reason
		}
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtIssueGaveUp,
		IssueID: iss.ID,
		Payload: payload,
	})
}

// GiveUpToRecord resolves a caller's stamp against the issue as it stands,
// returning the value to write and whether to write at all.
//
// A give-up describes a ticket that is still where the give-up PUT it. When
// the ticket has already moved — an operator got there between the terminal
// move and the stamp — the give-up is superseded, and recording it would put
// the operator's own choice under a "the dispatcher gave up and filed this
// ticket as …" banner. Nothing is written; the state change already stands in
// the audit log.
//
// A stamp arriving without a state is filled in from the issue, so the value
// compared for idempotence and the value written are always the same thing.
func GiveUpToRecord(iss *Issue, g *GiveUp) (*GiveUp, bool) {
	if g == nil {
		return nil, true
	}
	out := *g
	if out.State == "" {
		out.State = iss.State
	}
	if out.State != iss.State {
		return nil, false
	}
	return &out, true
}

// SameGiveUp compares two stamps on the fields that decide behaviour — the
// timestamp is provenance, not identity, so a re-stamp of the same give-up
// stays a no-op instead of churning the card's UpdatedAt on every poll.
func SameGiveUp(a, b *GiveUp) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.RunID == b.RunID && a.State == b.State && a.Attempts == b.Attempts && a.Reason == b.Reason && a.Launch == b.Launch
}

// AddComment appends a note to the issue's discussion thread and returns
// the updated issue plus the created comment. Author is a free-form
// display name; body must be non-empty. The append is persisted to
// issues/<id>.json and an EvtIssueComment record is emitted so external
// tailers (studio, webhook bridge) observe new comments.
func (s *Store) AddComment(id, author, body string) (updated *Issue, comment *Comment, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("AddComment", &err)
	if strings.TrimSpace(body) == "" {
		return nil, nil, errors.New("comment: body required")
	}
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, nil, err
	}
	c := Comment{
		ID:        uuid.NewString(),
		Author:    author,
		Body:      body,
		CreatedAt: time.Now().UTC(),
	}
	iss.Comments = append(iss.Comments, c)
	iss.UpdatedAt = c.CreatedAt
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, nil, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueComment,
		IssueID: id,
		Payload: map[string]any{"comment_id": c.ID, "author": author},
	}); err != nil {
		return nil, nil, err
	}
	return cloneIssue(iss), &c, nil
}

// Release clears the claim if it matches the given marker. Releasing an
// already-unclaimed issue is a no-op.
func (s *Store) Release(id, marker string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Release", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	if iss.Claim == "" {
		return nil
	}
	if iss.Claim != marker {
		return fmt.Errorf("%w: held by %s", tracker.ErrClaimConflict, iss.Claim)
	}
	return s.releaseLocked(iss, marker)
}

// releaseLocked clears the claim AND its lease bookkeeping — a released
// card must not keep a fossil lease a reaper could misread.
func (s *Store) releaseLocked(iss *Issue, marker string) error {
	iss.Claim = ""
	iss.ClaimedAt = time.Time{}
	iss.ClaimLeaseUntil = time.Time{}
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type: EvtIssueReleased, IssueID: iss.ID,
		Payload: map[string]any{"marker": marker},
	})
}
