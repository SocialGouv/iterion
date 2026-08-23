package native

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/google/uuid"
)

// Claim sets the claim marker. Returns tracker.ErrClaimConflict if the
// issue is already claimed by a different marker. Idempotent for the
// same marker.
func (s *Store) Claim(id, marker string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Claim", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return err
	}
	if iss.Claim != "" && iss.Claim != marker {
		return fmt.Errorf("%w: held by %s", tracker.ErrClaimConflict, iss.Claim)
	}
	if iss.Claim == marker {
		return nil
	}
	iss.Claim = marker
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type: EvtIssueClaimed, IssueID: id,
		Payload: map[string]any{"marker": marker},
	})
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
	if iss.LastRunID == runID && iss.LastWorkdir == workdir {
		return nil
	}
	now := time.Now().UTC()
	iss.LastRunID = runID
	iss.LastWorkdir = workdir
	iss.Runs = AppendRunRef(iss.Runs, runID, workdir, now)
	iss.UpdatedAt = now
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type:    EvtIssueLastRun,
		IssueID: id,
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
		IssueID: id,
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
	if sameGiveUp(iss.GaveUp, g) {
		return nil
	}
	if g == nil {
		iss.GaveUp = nil
	} else {
		stamp := *g
		if stamp.At.IsZero() {
			stamp.At = time.Now().UTC()
		}
		iss.GaveUp = &stamp
	}
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	payload := map[string]any{"gave_up": g != nil}
	if g != nil {
		payload["run_id"] = g.RunID
		payload["state"] = g.State
		payload["attempts"] = g.Attempts
	}
	return s.emitPostCommitEvent(Event{
		Type:    EvtIssueGaveUp,
		IssueID: id,
		Payload: payload,
	})
}

// sameGiveUp compares two stamps on the fields that decide behaviour — the
// timestamp is provenance, not identity, so a re-stamp of the same give-up
// stays a no-op instead of churning the card's UpdatedAt on every poll.
func sameGiveUp(a, b *GiveUp) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.RunID == b.RunID && a.State == b.State && a.Attempts == b.Attempts
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
	iss.Claim = ""
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return err
	}
	s.index[iss.ID] = cloneIssue(iss)
	return s.emitPostCommitEvent(Event{
		Type: EvtIssueReleased, IssueID: id,
		Payload: map[string]any{"marker": marker},
	})
}
