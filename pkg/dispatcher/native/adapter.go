package native

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// Adapter exposes a BoardStore under the tracker.Tracker interface so the
// dispatcher can dispatch board issues with the same code path that drives
// external trackers (GitHub, Forgejo). It uses only BoardStore methods, so it
// wraps either the filesystem *Store (self-hosted) or the Mongo store
// (boardmongo, cloud) unchanged.
type Adapter struct {
	store BoardStore
}

// NewAdapter wraps a board store as a tracker.Tracker.
func NewAdapter(store BoardStore) *Adapter { return &Adapter{store: store} }

// Name implements tracker.Tracker.
func (a *Adapter) Name() string { return "native" }

// ListCandidates returns unclaimed issues whose state is marked
// eligible on the board, excluding those whose hard blockers are not
// all StateDone (see BlockersSatisfied / CanLaunch). Missing blockers
// are treated as open (fail closed). Terminal non-success states such
// as StateBlocked do NOT satisfy a dependency.
// LaunchStates names the board columns a card is dispatched FROM — see
// tracker.LaunchStateLister. One definition, shared with ListCandidates
// below, so the watchdog and the poller can never disagree about which
// column a launch started in.
func (a *Adapter) LaunchStates() []string {
	b := a.store.Board()
	out := make([]string, 0, len(b.States))
	for _, s := range b.States {
		if s.Eligible {
			out = append(out, s.Name)
		}
	}
	return out
}

func (a *Adapter) ListCandidates(ctx context.Context) ([]tracker.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eligible := a.LaunchStates()
	if len(eligible) == 0 {
		return nil, nil
	}
	free := false
	issues, err := a.store.List(ListFilter{States: eligible, Claimed: &free})
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, iss := range issues {
		// Same hard-dep rule as the pipeline launch loop (CanLaunch's
		// blocker half, including optional require_blocker_labels).
		ok, _ := BlockersSatisfiedForIssue(a.store, iss)
		if !ok {
			continue
		}
		out = append(out, toTrackerIssue(iss))
	}
	return out, nil
}

// RefreshStates returns the current state for each requested ID;
// missing IDs are omitted.
func (a *Adapter) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		iss, err := a.store.Get(id)
		if err != nil {
			continue
		}
		out[id] = iss.State
	}
	return out, nil
}

// UpdateState delegates to the store.
func (a *Adapter) UpdateState(ctx context.Context, id, newState string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := a.store.SetState(id, newState)
	return err
}

// Comment appends a note to the issue's discussion thread under the
// "dispatcher" author — the dispatcher and finalize hooks are the callers
// that leave a trail (e.g. the MR/PR back-link a run posts at the end).
// Operator-authored comments arrive via the REST endpoint, which calls
// Store.AddComment directly with the operator as author.
func (a *Adapter) Comment(ctx context.Context, id, body string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, err := a.store.AddComment(id, "dispatcher", body)
	return err
}

// Claim delegates to the store (tracker.Tracker's tokenless form — the
// token is discarded here; token-aware callers use ClaimLease).
func (a *Adapter) Claim(ctx context.Context, id, marker string) error {
	_, err := a.ClaimLease(ctx, id, marker)
	return err
}

// ClaimLease claims and returns the ownership token (tracker.ClaimLeaser).
func (a *Adapter) ClaimLease(ctx context.Context, id, marker string) (tracker.ClaimToken, error) {
	if err := ctx.Err(); err != nil {
		return tracker.ClaimToken{}, err
	}
	return a.store.Claim(id, marker)
}

// RenewClaim delegates to the store (tracker.ClaimLeaser), honouring the
// caller's cancel for the whole call and not just at its entry.
//
// The store's renew takes no context and blocks on the store-wide lock,
// so any long mutation holding it (a sweep, a bulk column migration)
// blocks the heartbeat here. claimSession.Stop() runs ON THE ACTOR and
// waits for that loop — so checking ctx only on the way in made the
// session's documented "Stop() is not hostage to a slow renewal"
// guarantee false on this backend, and left its context.Canceled arm
// unreachable from the dispatcher (the cloud twin already honours it,
// through RenewClaimCtx).
//
// The goroutine ends when the lock frees; the channel is buffered so it
// never blocks on a caller that has gone. A renewal that lands after the
// cancel merely extends a lease we still own — Stop() is followed by the
// release, which is fenced on the same token.
func (a *Adapter) RenewClaim(ctx context.Context, id string, tok tracker.ClaimToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- a.store.RenewClaim(id, tok) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseOwned delegates to the store (tracker.ClaimLeaser).
func (a *Adapter) ReleaseOwned(ctx context.Context, id string, tok tracker.ClaimToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.store.ReleaseOwned(id, tok)
}

// UpdateStateOwned delegates to the store (tracker.ClaimLeaser).
func (a *Adapter) UpdateStateOwned(ctx context.Context, id, newState string, tok tracker.ClaimToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := a.store.SetStateOwned(id, newState, tok)
	return err
}

// UpdateStateOwnedFrom is the fenced CAS move with a source-state
// precondition (BoardStore.SetStateOwnedFrom): the finish worker's
// auto-transition is ONE write through it, never a state probe followed
// by a fenced overwrite. changed=false means the card drifted and nothing
// was written.
func (a *Adapter) UpdateStateOwnedFrom(ctx context.Context, id, from, to string, tok tracker.ClaimToken) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, changed, err := a.store.SetStateOwnedFrom(id, from, to, tok)
	return changed, err
}

// UpdateStateOwnedReason is the fenced state write carrying an explicit
// provenance (the watchdog's terminal verdicts) — see
// native.SetStateOwnedReason.
func (a *Adapter) UpdateStateOwnedReason(ctx context.Context, id, newState string, tok tracker.ClaimToken, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rr, ok := a.store.(interface {
		SetStateOwnedReason(id, newState string, tok tracker.ClaimToken, reason string) (*Issue, error)
	})
	if !ok {
		// Backend without the reasoned form: marker-derived provenance.
		_, err := a.store.SetStateOwned(id, newState, tok)
		return err
	}
	_, err := rr.SetStateOwnedReason(id, newState, tok, reason)
	return err
}

// SetLastRunOwned / SetAwaitingInputOwned / SetGaveUpOwned are the fenced
// forms of the setter pass-throughs below — same regression-guard rule:
// a store method without its Adapter pass-through fails silently at the
// optional-interface assertion.
func (a *Adapter) SetLastRunOwned(id, runID, workdir string, tok tracker.ClaimToken) error {
	return a.store.SetLastRunOwned(id, runID, workdir, tok)
}

func (a *Adapter) SetAwaitingInputOwned(id string, v bool, tok tracker.ClaimToken) error {
	return a.store.SetAwaitingInputOwned(id, v, tok)
}

func (a *Adapter) SetGaveUpOwned(id string, g *GiveUp, tok tracker.ClaimToken) error {
	return a.store.SetGaveUpOwned(id, g, tok)
}

// ListExpiredClaimCandidates / ReclaimExpired expose the store's reaper
// half under tracker.ClaimReaper (compile-asserted below).
func (a *Adapter) ListExpiredClaimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]tracker.ExpiredClaim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.store.ListExpiredClaimCandidates(cutoff, limit)
}

func (a *Adapter) ReclaimExpired(ctx context.Context, id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tracker.ClaimToken, string, error) {
	if err := ctx.Err(); err != nil {
		return tracker.ClaimToken{}, "", err
	}
	return a.store.ReclaimExpired(id, prev, marker, cutoff)
}

// Release delegates to the store.
func (a *Adapter) Release(ctx context.Context, id, marker string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.store.Release(id, marker)
}

// ListForRepark returns cards this dispatcher may re-park after a
// reboot or an out-of-band resume: any eligible state (plus
// in_progress, the crash-recovery lane) that already has a last_run
// and is unclaimed. Cards already claimed by this daemon were handled when
// the claim was acquired; excluding them makes a missing awaiting_input
// column a one-shot best-effort move instead of an every-tick retry. Cards
// already in
// awaiting_input stay with ListAwaitingInput / reconcileParked.
// Consumed by reconcileStrandedPaused via optional-interface assertion.
func (a *Adapter) ListForRepark(_ string) ([]tracker.Issue, error) {
	b := a.store.Board()
	seen := make(map[string]bool, len(b.States))
	states := make([]string, 0, len(b.States))
	for _, s := range b.States {
		if !s.Eligible && s.Name != StateInProgress {
			continue
		}
		if s.Name == StateAwaitingInput || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		states = append(states, s.Name)
	}
	if len(states) == 0 {
		return nil, nil
	}
	issues, err := a.store.List(ListFilter{States: states})
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, iss := range issues {
		if iss.LastRunID == "" {
			continue
		}
		if iss.Claim != "" {
			continue
		}
		out = append(out, toTrackerIssue(iss))
	}
	return out, nil
}

// ListAwaitingInput returns the cards parked in the awaiting-input
// column that this dispatcher may reconcile: unclaimed (post-restart,
// after the stale-claim sweep) or claimed by the given marker. Cards
// claimed by ANOTHER live daemon sharing the store are excluded — their
// owner reconciles them. Consumed by the dispatcher's parked sweep
// (reconcileParked in pkg/dispatcher) via optional-interface assertion,
// same seam as SetLastRun/SetAwaitingInput.
func (a *Adapter) ListAwaitingInput(marker string) ([]tracker.Issue, error) {
	issues, err := a.store.List(ListFilter{States: []string{StateAwaitingInput}})
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, iss := range issues {
		if iss.Claim != "" && iss.Claim != marker {
			continue
		}
		out = append(out, toTrackerIssue(iss))
	}
	return out, nil
}

// SetLastRun stamps the (runID, workdir) pair on the issue so the
// dispatcher can pivot from a kanban card back to the run that
// processed it (studio's IssueModal, the resume fallback in
// loop.go). Phase 3 (commit 9835ae29) added Store.SetLastRun but
// forgot the Adapter pass-through — without this method the
// dispatcher's type assertion in stampLastRun fell through silently
// and no issue ever got its LastRunID populated. Adapter exposes
// the method so c.tracker.(SetLastRun-interface) resolves.
func (a *Adapter) SetLastRun(id, runID, workdir string) error {
	return a.store.SetLastRun(id, runID, workdir)
}

// SetAwaitingInput passes through to the underlying store so the
// dispatcher's optional-interface type assertion (setAwaitingInput in
// commands.go) resolves — without this pass-through the seam falls
// through silently and no card ever gets its awaiting-input badge (the
// SetLastRun regression above, avoided here).
func (a *Adapter) SetAwaitingInput(id string, v bool) error {
	return a.store.SetAwaitingInput(id, v)
}

// SetGaveUp passes through to the underlying store so the dispatcher's
// optional-interface type assertion (setGaveUp in commands.go) resolves —
// same seam, and same silent-fall-through trap, as SetAwaitingInput above.
func (a *Adapter) SetGaveUp(id string, g *GiveUp) error {
	return a.store.SetGaveUp(id, g)
}

// LastRunForIssue returns the runID of the most recent dispatch on
// this issue. Empty when the issue has never been dispatched (or the
// issue does not exist). Used by the dispatcher's resume path as a
// cross-daemon-restart fallback: in-memory retry entries vanish when
// the daemon restarts, but the issue record on disk preserves
// LastRunID so the next dispatch can still find the prior run and
// resume from its checkpoint.
func (a *Adapter) LastRunForIssue(id string) (string, error) {
	iss, err := a.store.Get(id)
	if err != nil {
		return "", err
	}
	return iss.LastRunID, nil
}

// SweepStaleClaims walks every claimed issue and releases the claim
// when isStale reports true for its marker. Returns the issue IDs whose
// claim was cleared. Caller-supplied predicate keeps PID/host knowledge
// out of the native store — typically the dispatcher passes a callback
// that recognises its own "<hostname>-<pid>" markers and probes the
// local kernel for a live process.
func (a *Adapter) SweepStaleClaims(isStale func(marker string) bool) ([]string, error) {
	if isStale == nil {
		return nil, nil
	}
	claimed := true
	issues, err := a.store.List(ListFilter{Claimed: &claimed})
	if err != nil {
		return nil, err
	}
	var cleared []string
	skipped, attempted := 0, 0
	for _, iss := range issues {
		if iss.Claim == "" {
			continue
		}
		if !isStale(iss.Claim) {
			continue
		}
		attempted++
		if err := a.store.Release(iss.ID, iss.Claim); err != nil {
			// The listing is a snapshot, not the truth at the moment of
			// the act: losing the race on ONE card is benign (the reaper
			// transferred it, a peer released it, the card was deleted) —
			// aborting here left every FOLLOWING card claimed by a dead
			// PID until the next boot, and this sweep is the only ungated
			// repair that population has. Same tolerance as
			// sweepJournalledClaims, its structural sibling.
			if errors.Is(err, tracker.ErrClaimConflict) || errors.Is(err, tracker.ErrNotFound) {
				skipped++
				continue
			}
			return cleared, fmt.Errorf("release stale claim on %s (%s): %w", iss.ID, iss.Claim, err)
		}
		cleared = append(cleared, iss.ID)
	}
	if skipped > 0 && len(cleared) == 0 && skipped == attempted {
		// EVERY stale claim lost its race: sweep-wide contention is a
		// different condition from a per-card blip, and a silent
		// (nil, nil) here would read as "nothing was stale". Counted in
		// the loop — re-invoking the predicate here doubled the boot's
		// PID probes and let a time-varying predicate suppress the very
		// diagnostic this exists to emit.
		return nil, fmt.Errorf("stale-claim sweep released nothing: all %d candidates lost their release race", skipped)
	}
	return cleared, nil
}

func toTrackerIssue(iss *Issue) tracker.Issue {
	return tracker.Issue{
		ID:            iss.ID,
		Identifier:    shortIdentifier(iss.ID),
		Title:         iss.Title,
		Body:          iss.Body,
		WorkflowState: iss.State,
		Priority:      iss.Priority,
		CreatedAt:     iss.CreatedAt,
		UpdatedAt:     iss.UpdatedAt,
		Labels:        append([]string(nil), iss.Labels...),
		Assignee:      iss.Assignee,
		Blockers:      append([]string(nil), iss.Blockers...),
		Fields:        maps.Clone(iss.Fields),
		Bot:           iss.Bot,
		BotArgs:       maps.Clone(iss.BotArgs),
	}
}

func shortIdentifier(id string) string {
	if len(id) <= 15 {
		return id
	}
	return id[:15]
}

// Compile-time: the adapter exposes BOTH watchdog capabilities — an
// optional-interface miss here is the documented SetLastRun regression
// (a store method without its Adapter pass-through fails silently).
var (
	_ tracker.ClaimLeaser = (*Adapter)(nil)
	_ tracker.ClaimReaper = (*Adapter)(nil)
)
