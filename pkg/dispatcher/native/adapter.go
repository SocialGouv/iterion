package native

import (
	"context"
	"fmt"
	"maps"

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
func (a *Adapter) ListCandidates(ctx context.Context) ([]tracker.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := a.store.Board()
	eligible := make([]string, 0, len(b.States))
	for _, s := range b.States {
		if s.Eligible {
			eligible = append(eligible, s.Name)
		}
	}
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

// Claim delegates to the store.
func (a *Adapter) Claim(ctx context.Context, id, marker string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.store.Claim(id, marker)
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
	for _, iss := range issues {
		if iss.Claim == "" {
			continue
		}
		if !isStale(iss.Claim) {
			continue
		}
		if err := a.store.Release(iss.ID, iss.Claim); err != nil {
			return cleared, fmt.Errorf("release stale claim on %s (%s): %w", iss.ID, iss.Claim, err)
		}
		cleared = append(cleared, iss.ID)
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
