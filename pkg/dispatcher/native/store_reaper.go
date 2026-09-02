package native

import (
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// The reaper half of the claim lease (filesystem twin): list the cards
// whose lease ran out, and CAS-TRANSFER one to a recovery owner. The
// transfer — never a bare clear — is what closes the double-launch
// window: an eligible-state card freed before its disposition is
// decided is instantly re-dispatchable by the very tick the reaper is
// cleaning up after.

// ListExpiredClaimCandidates — see tracker.ClaimReaper. Legacy claims
// (no lease ever stamped) are never listed: time proves nothing about
// them, only the historical same-host pid-probe sweep may touch them.
func (s *Store) ListExpiredClaimCandidates(cutoff time.Time, limit int) ([]tracker.ExpiredClaim, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []tracker.ExpiredClaim
	for _, iss := range s.index {
		if len(out) >= limit {
			break
		}
		if iss.Claim == "" || iss.ClaimLeaseUntil.IsZero() || !iss.ClaimLeaseUntil.Before(cutoff) {
			continue
		}
		out = append(out, tracker.ExpiredClaim{
			IssueID:    iss.ID,
			Identifier: shortIdentifier(iss.ID),
			State:      iss.State,
			LastRunID:  iss.LastRunID,
			Prev:       tracker.ClaimToken{Marker: iss.Claim, Epoch: iss.ClaimEpoch},
		})
	}
	return out, nil
}

// ReclaimExpired — see tracker.ClaimReaper. One critical section: the
// claim must still be exactly prev AND still expired, then the transfer
// bumps the epoch (the dead owner's late writes die at the fence) and
// stamps a fresh lease for the recovery owner.
func (s *Store) ReclaimExpired(id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tok tracker.ClaimToken, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("ReclaimExpired", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return tracker.ClaimToken{}, err
	}
	if iss.Claim != prev.Marker || iss.ClaimEpoch != prev.Epoch ||
		iss.ClaimLeaseUntil.IsZero() || !iss.ClaimLeaseUntil.Before(cutoff) {
		return tracker.ClaimToken{}, fmt.Errorf("%w: claim moved on (now %q epoch %d, lease until %s)",
			tracker.ErrClaimConflict, iss.Claim, iss.ClaimEpoch, iss.ClaimLeaseUntil.Format(time.RFC3339))
	}
	now := time.Now().UTC()
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
		Payload: map[string]any{"marker": marker, "claim_epoch": iss.ClaimEpoch, "reclaimed_from": prev.Marker},
	}); err != nil {
		return tracker.ClaimToken{}, err
	}
	return tracker.ClaimToken{Marker: marker, Epoch: iss.ClaimEpoch}, nil
}
