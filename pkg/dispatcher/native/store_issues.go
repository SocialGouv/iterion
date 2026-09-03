package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/google/uuid"
)

// Create persists a new issue. The State must be one of the configured
// board states; if empty, the first state is used. ID is generated if
// missing.
func (s *Store) Create(in Issue) (created *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Create", &err)
	return s.createLocked(in)
}

// CreateUniqueTitle creates an issue whose title is made distinct from
// every existing title WITHIN THE SAME critical section as the write, so
// two concurrent creates of the same desired title cannot both land it
// (PR #193 review M4 — the previous list-then-check was racy). When the
// desired title is free it is used verbatim; otherwise the smallest free
// "#N - " prefix (N≥2) is prepended, kept as a PREFIX so the counter stays
// visible under title truncation.
func (s *Store) CreateUniqueTitle(in Issue, normalize func(string) string) (created *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("CreateUniqueTitle", &err)
	in.Title = s.uniqueTitleLocked(strings.TrimSpace(in.Title), normalize)
	return s.createLocked(in)
}

// uniqueTitleLocked returns desired if no issue already holds it, else the
// smallest free "#N - desired" (N≥2). When normalize is non-nil it is applied to
// each candidate, so a caller's rune-aware truncator keeps a prefixed title
// within its display budget (the "#N - " head survives truncation, so distinct N
// stay distinct). Caller must hold s.mu.
func (s *Store) uniqueTitleLocked(desired string, normalize func(string) string) string {
	norm := func(x string) string {
		if normalize != nil {
			return normalize(x)
		}
		return x
	}
	desired = norm(desired)
	taken := make(map[string]struct{}, len(s.index))
	for _, iss := range s.index {
		if iss != nil {
			taken[iss.Title] = struct{}{}
		}
	}
	if _, clash := taken[desired]; !clash {
		return desired
	}
	for n := 2; n < 100000; n++ {
		candidate := norm(fmt.Sprintf("#%d - %s", n, desired))
		if _, clash := taken[candidate]; !clash {
			return candidate
		}
	}
	return desired
}

// createLocked is the body of Create; the caller holds s.mu and installs
// the recoverMutator guard.
func (s *Store) createLocked(in Issue) (created *Issue, err error) {
	if in.Title == "" {
		return nil, errors.New("issue: title required")
	}
	if in.State == "" {
		in.State = s.board.States[0].Name
	}
	if s.board.StateByName(in.State) == nil {
		return nil, fmt.Errorf("issue: unknown state %q", in.State)
	}
	if err := s.board.ValidateFieldValues(in.Fields); err != nil {
		return nil, err
	}
	in.Blockers = NormalizeBlockers(in.Blockers)
	in.ParentID = strings.TrimSpace(in.ParentID)

	if in.ID == "" {
		in.ID = "native:" + uuid.NewString()
	} else if err := validateIssueID(in.ID); err != nil {
		return nil, err
	}
	if _, exists := s.index[in.ID]; exists {
		return nil, fmt.Errorf("issue: id %q already exists", in.ID)
	}
	if in.ParentID == in.ID {
		return nil, errors.New("issue: parent_id cannot be self")
	}
	// Cycle check once the final id is known (catches self-ref + A→B→A).
	if err := s.validateBlockersLocked(in.ID, in.Blockers); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	in.CreatedAt = now
	in.UpdatedAt = now
	if err := s.writeIssueLocked(&in); err != nil {
		return nil, err
	}
	s.index[in.ID] = cloneIssue(&in)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueCreated,
		IssueID: in.ID,
		Payload: map[string]any{"state": in.State, "title": in.Title},
	}); err != nil {
		return nil, err
	}
	if len(in.Blockers) > 0 {
		if err := s.emitPostCommitEvent(Event{
			Type:    EvtIssueBlockersUpdated,
			IssueID: in.ID,
			Payload: map[string]any{"blockers": append([]string(nil), in.Blockers...)},
		}); err != nil {
			return nil, err
		}
	}
	clone := in
	return &clone, nil
}

// Get returns a defensive copy of the issue with the given ID.
func (s *Store) Get(id string) (*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if iss, ok := s.index[id]; ok {
		return cloneIssue(iss), nil
	}
	return nil, tracker.ErrNotFound
}

// ListFilter constrains the result of List. Zero-value fields don't filter.
type ListFilter struct {
	States   []string
	Labels   []string
	Assignee string
	Claimed  *bool
}

// List returns defensive copies of issues matching the filter, sorted
// by priority desc, then created_at asc. Walks the in-memory index —
// no filesystem I/O on the hot path.
//
// Note: every match incurs a full cloneIssue under the store mutex.
// At the current sub-1k-issue usage this is invisible; once a board
// holds more than ~1k open issues the dispatcher poller (which calls
// List on every tick) starts to contend with mutators. The cheap
// remediation is to filter-and-count first under the read lock, drop
// the lock, then clone outside it — defer until benchmarks show real
// contention.
func (s *Store) List(filter ListFilter) ([]*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Issue, 0, len(s.index))
	for _, iss := range s.index {
		if !filter.match(iss) {
			continue
		}
		out = append(out, cloneIssue(iss))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// cloneIssue returns a deep copy of an issue so callers receive their
// own mutable instance and cannot mutate the in-memory cache.
func cloneIssue(in *Issue) *Issue {
	c := *in
	if in.External != nil {
		ext := *in.External
		c.External = &ext
	}
	if in.GaveUp != nil {
		g := *in.GaveUp
		c.GaveUp = &g
	}
	if in.Labels != nil {
		c.Labels = append([]string(nil), in.Labels...)
	}
	if in.Blockers != nil {
		c.Blockers = append([]string(nil), in.Blockers...)
	}
	if in.Fields != nil {
		c.Fields = make(map[string]any, len(in.Fields))
		for k, v := range in.Fields {
			c.Fields[k] = v
		}
	}
	if in.BotArgs != nil {
		c.BotArgs = make(map[string]string, len(in.BotArgs))
		for k, v := range in.BotArgs {
			c.BotArgs[k] = v
		}
	}
	if in.Comments != nil {
		c.Comments = append([]Comment(nil), in.Comments...)
	}
	if in.Runs != nil {
		c.Runs = append([]RunRef(nil), in.Runs...)
	}
	return &c
}

func (f ListFilter) match(iss *Issue) bool {
	if len(f.States) > 0 && !slices.Contains(f.States, iss.State) {
		return false
	}
	for _, want := range f.Labels {
		if !slices.Contains(iss.Labels, want) {
			return false
		}
	}
	if f.Assignee != "" && iss.Assignee != f.Assignee {
		return false
	}
	if f.Claimed != nil {
		hasClaim := iss.Claim != ""
		if *f.Claimed != hasClaim {
			return false
		}
	}
	return true
}

// Patch describes a partial update to an issue. Pointer fields are nil
// when the corresponding field is not being changed.
type Patch struct {
	Title    *string
	Body     *string
	Labels   *[]string
	Priority *int
	Assignee *string
	Blockers *[]string
	// ParentID, when non-nil, sets the planner provenance pointer (empty
	// string clears it). Distinct from Blockers.
	ParentID *string
	// Fields is merged into the issue's Fields. A nil value deletes the key.
	Fields map[string]any
	// Bot, when non-nil, sets the per-ticket bot override (empty string
	// clears it). The dispatcher resolves it to a workflow at launch.
	Bot *string
	// BotArgs, when non-nil, replaces the issue's bot args wholesale
	// (a nil map deletes; an empty map clears with no entries). This
	// mirrors how Labels and Blockers are handled — the entire
	// collection swaps. Per-key partial updates aren't useful because
	// the studio always sends the full form state.
	BotArgs *map[string]string
	// External, when non-nil, sets the card's forge linkage (the
	// forge→board sync worker refreshes url/state; push-to-forge stamps a
	// previously-unlinked card). A nil pointer leaves the existing link.
	External *ExternalRef
}

// Update applies the patch and emits an issue_updated event with the
// list of changed top-level fields. State changes are not supported here;
// use SetState.
func (s *Store) Update(id string, p Patch) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Update", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, err
	}
	changed := []string{}
	if p.Title != nil && *p.Title != iss.Title {
		iss.Title = *p.Title
		changed = append(changed, "title")
	}
	if p.Body != nil && *p.Body != iss.Body {
		iss.Body = *p.Body
		changed = append(changed, "body")
	}
	if p.Labels != nil {
		iss.Labels = append([]string(nil), (*p.Labels)...)
		changed = append(changed, "labels")
	}
	if p.Priority != nil && *p.Priority != iss.Priority {
		iss.Priority = *p.Priority
		changed = append(changed, "priority")
	}
	if p.Assignee != nil && *p.Assignee != iss.Assignee {
		iss.Assignee = *p.Assignee
		changed = append(changed, "assignee")
	}
	if p.ParentID != nil && *p.ParentID != iss.ParentID {
		pid := strings.TrimSpace(*p.ParentID)
		if pid == id {
			return nil, errors.New("issue: parent_id cannot be self")
		}
		iss.ParentID = pid
		changed = append(changed, "parent_id")
	}
	blockersChanged := false
	if p.Blockers != nil {
		next := NormalizeBlockers(*p.Blockers)
		if err := s.validateBlockersLocked(id, next); err != nil {
			return nil, err
		}
		iss.Blockers = next
		changed = append(changed, "blockers")
		blockersChanged = true
	}
	if len(p.Fields) > 0 {
		merged := map[string]any{}
		for k, v := range iss.Fields {
			merged[k] = v
		}
		for k, v := range p.Fields {
			if v == nil {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if err := s.board.ValidateFieldValues(merged); err != nil {
			return nil, err
		}
		iss.Fields = merged
		changed = append(changed, "fields")
	}
	if p.External != nil {
		ext := *p.External
		iss.External = &ext
		changed = append(changed, "external")
	}
	if p.Bot != nil && *p.Bot != iss.Bot {
		iss.Bot = *p.Bot
		changed = append(changed, "bot")
	}
	if p.BotArgs != nil {
		var next map[string]string
		if len(*p.BotArgs) > 0 {
			next = make(map[string]string, len(*p.BotArgs))
			for k, v := range *p.BotArgs {
				next[k] = v
			}
		}
		iss.BotArgs = next
		changed = append(changed, "bot_args")
	}
	if len(changed) == 0 {
		return iss, nil
	}
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueUpdated,
		IssueID: iss.ID,
		Payload: map[string]any{"changed": changed},
	}); err != nil {
		return nil, err
	}
	if blockersChanged {
		if err := s.emitPostCommitEvent(Event{
			Type:    EvtIssueBlockersUpdated,
			IssueID: iss.ID,
			Payload: map[string]any{"blockers": append([]string(nil), iss.Blockers...)},
		}); err != nil {
			return nil, err
		}
	}
	return iss, nil
}

// SetState transitions an issue, validating against the board. Returns
// tracker.ErrTransitionRejected if newState is unknown. When the new
// state is StateDone, dependents parked in StateWaitingDeps whose hard
// blockers are now all satisfied are auto-promoted (default → backlog,
// or → ready when bot_args.auto_ready is set) and emit issue_unblocked.
func (s *Store) SetState(id, newState string) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetState", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, err
	}
	return s.setStateLocked(iss, newState, "")
}

// SetStateOwned is SetState fenced on the claim token — the transition
// an owning worker performs while it still holds the card (one critical
// section: check-then-call would reopen the TOCTOU the fence closes).
func (s *Store) SetStateOwned(id, newState string, tok tracker.ClaimToken) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetStateOwned", &err)
	iss, err := s.ownedIssueLocked(id, tok)
	if err != nil {
		return nil, err
	}
	return s.setStateLocked(iss, newState, tok.Marker)
}

// SetStateOwnedReason is SetStateOwned with an EXPLICIT provenance
// overriding the marker-derived one — the watchdog's terminal filings
// carry the run's own verdict (run_finished / run_failed, descriptive)
// so the card's downstream chain fires as it would have for the living
// owner; its reparks keep the marker-derived machine reason.
func (s *Store) SetStateOwnedReason(id, newState string, tok tracker.ClaimToken, reason string) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetStateOwnedReason", &err)
	iss, err := s.ownedIssueLocked(id, tok)
	if err != nil {
		return nil, err
	}
	// The zero value falls back to the marker-derived provenance — the
	// SEAM decides, not a guard recopied at every call site: without
	// this the twins diverged on reason=="" (Mongo marker-derived, FS
	// none), a trap armed for the first caller that passes it through.
	if reason == "" {
		return s.setStateLocked(iss, newState, tok.Marker)
	}
	return s.setStateReasonLocked(iss, newState, "", reason)
}

// Reopen is the ONE sanctioned exit from a terminal state — an
// operator-surface op, refused when dependents were already promoted on
// this card's completion (deterministic v1). It emits the ordinary
// state event (tailers and the trigger spine must see the truth) with a
// reopened marker.
func (s *Store) Reopen(id, toState string) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Reopen", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, err
	}
	st := s.board.StateByName(iss.State)
	if st == nil || !st.Terminal {
		return nil, fmt.Errorf("%w: %q is not terminal — use an ordinary state move", tracker.ErrTransitionRejected, iss.State)
	}
	if s.board.StateByName(toState) == nil {
		return nil, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, toState)
	}
	if to := s.board.StateByName(toState); to.Terminal && toState != iss.State {
		return nil, fmt.Errorf("%w: reopen targets a working state, not another terminal (%q)", tracker.ErrTransitionRejected, toState)
	}
	all := make([]*Issue, 0, len(s.index))
	for _, dep := range s.index {
		all = append(all, dep)
	}
	if err := ReopenBlockedByDependents(all, id, iss.State); err != nil {
		return nil, err
	}
	old := iss.State
	iss.State = toState
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueState,
		IssueID: iss.ID,
		Payload: map[string]any{"from": old, "to": toState, "reopened": true},
	}); err != nil {
		return nil, err
	}
	return iss, nil
}

// SetStateFrom is the CAS form for AUTOMATED writers: the move lands
// only when the current state is exactly `from` (changed=false when it
// drifted — an operator got there first), and the terminal guard still
// applies (automation never exits a sink).
func (s *Store) SetStateFrom(id, from, to string) (updated *Issue, changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetStateFrom", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, false, err
	}
	// Guard BEFORE the drift check (twin contract): an automated writer
	// that declares a terminal source is a programming error and must be
	// refused loudly whatever the card currently reads.
	if from != to {
		if err := ValidateStateExit(s.board, from, to); err != nil {
			return nil, false, err
		}
	}
	if iss.State != from {
		return cloneIssue(iss), false, nil
	}
	if from == to {
		// Nothing to perform, so nothing was CHANGED. setStateLocked
		// already no-ops on the same state, but returning true for it made
		// the two twins disagree on the flag (Mongo returns false), and a
		// caller that reads changed==false as a refusal — the shape this
		// CAS invites — then behaved differently on each backend.
		return cloneIssue(iss), false, nil
	}
	out, err := s.setStateLocked(iss, to, "")
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// SetStateWithReason is SetState carrying an explicit DESCRIPTIVE
// provenance (StateReasoner) — the FS half of the twin contract the
// shared auto-promote writes through. Without it the exported
// PromoteUnblockedDependents silently degraded to the bare SetState on
// this twin (its other caller, the board deps surface) and the promote
// lost its reason here while Mongo stamped it.
func (s *Store) SetStateWithReason(id, newState, reason string) (updated *Issue, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("SetStateWithReason", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, err
	}
	return s.setStateReasonLocked(iss, newState, "", reason)
}

// byMarker is the WRITER's identity (the claim token a fenced write
// presented; "" for tokenless operator/automation writes) — provenance
// describes who acted, never who happens to hold the card.
func (s *Store) setStateLocked(iss *Issue, newState, byMarker string) (*Issue, error) {
	return s.setStateReasonLocked(iss, newState, byMarker, "")
}

func (s *Store) setStateReasonLocked(iss *Issue, newState, byMarker, reason string) (*Issue, error) {
	if s.board.StateByName(newState) == nil {
		return nil, fmt.Errorf("%w: unknown state %q", tracker.ErrTransitionRejected, newState)
	}
	if iss.State == newState {
		return iss, nil
	}
	if err := ValidateStateExit(s.board, iss.State, newState); err != nil {
		return nil, err
	}
	old := iss.State
	iss.State = newState
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueState,
		IssueID: iss.ID,
		Payload: func() map[string]any {
			p := StateChangePayload(old, newState, byMarker)
			if reason != "" {
				p["reason"] = reason
			}
			return p
		}(),
	}); err != nil {
		return nil, err
	}
	if newState == StateDone {
		// Best-effort: a failed auto-promote must not roll back the
		// successful transition that just committed.
		_ = s.promoteUnblockedDependentsLocked(iss.ID)
	}
	return iss, nil
}

// ClaimForLaunch atomically transitions a ticket from StateReady to
// StateInProgress and reports whether THIS caller won the transition. It
// closes the check-then-act window (PR #193 M2) where a live dispatcher and
// the studio admission loop both pick the same Ready ticket: under the lock,
// exactly one caller observes state == StateReady, flips it, and returns
// (issue, true, nil); every other caller returns (nil, false, nil). A ticket
// that is not in StateReady is not claimable — (nil, false, nil), not an error.
func (s *Store) ClaimForLaunch(id string) (claimed *Issue, won bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("ClaimForLaunch", &err)
	iss, err := s.readIssueLocked(id)
	if err != nil {
		return nil, false, err
	}
	if iss.State != StateReady {
		return nil, false, nil
	}
	// A claimed card already has a launcher: the dispatcher wins with the
	// CLAIM (its move to in_progress is offloaded off the actor), so the
	// state alone cannot say the card is free. Admitting it here made
	// this a second launch authority and double-launched the card.
	if iss.Claim != "" {
		return nil, false, nil
	}
	iss.State = StateInProgress
	iss.UpdatedAt = time.Now().UTC()
	if err := s.writeIssueLocked(iss); err != nil {
		return nil, false, err
	}
	s.index[iss.ID] = cloneIssue(iss)
	if err := s.emitPostCommitEvent(Event{
		Type:    EvtIssueState,
		IssueID: iss.ID,
		Payload: map[string]any{"from": StateReady, "to": StateInProgress},
	}); err != nil {
		return nil, false, err
	}
	return iss, true, nil
}

// validateBlockersLocked rejects cycles against the in-memory index. Caller
// must hold s.mu. Uses a locked IssueGetter so Get is not re-entered.
func (s *Store) validateBlockersLocked(id string, blockers []string) error {
	return ValidateBlockers(lockedIssueGetter{s: s}, id, blockers)
}

// promoteUnblockedDependentsLocked walks every issue that lists closedID as
// a blocker. Those currently in StateWaitingDeps with all blockers now
// satisfied are moved to UnblockTarget and emit EvtIssueUnblocked. Caller
// holds s.mu.
func (s *Store) promoteUnblockedDependentsLocked(closedID string) error {
	if closedID == "" || s.board.StateByName(StateWaitingDeps) == nil {
		return nil
	}
	// Snapshot IDs first — promote mutates the index.
	var candidates []string
	for id, iss := range s.index {
		if iss == nil || iss.State != StateWaitingDeps {
			continue
		}
		for _, b := range iss.Blockers {
			if b == closedID {
				candidates = append(candidates, id)
				break
			}
		}
	}
	g := lockedIssueGetter{s: s}
	for _, id := range candidates {
		iss := s.index[id]
		if iss == nil || iss.State != StateWaitingDeps {
			continue
		}
		ok, _ := BlockersSatisfiedForIssue(g, iss)
		if !ok {
			continue
		}
		target := UnblockTarget(s.board, iss)
		if target == "" || target == iss.State {
			continue
		}
		if s.board.StateByName(target) == nil {
			continue
		}
		from := iss.State
		// Mutate a clone then write — index holds shared pointers.
		next := cloneIssue(iss)
		next.State = target
		next.UpdatedAt = time.Now().UTC()
		if err := s.writeIssueLocked(next); err != nil {
			return err
		}
		s.index[id] = cloneIssue(next)
		if err := s.emitPostCommitEvent(Event{
			Type:    EvtIssueUnblocked,
			IssueID: id,
			Payload: map[string]any{
				"from":           from,
				"to":             target,
				"closed_blocker": closedID,
			},
		}); err != nil {
			return err
		}
		// Also emit a state-changed for consumers that only watch that type.
		if err := s.emitPostCommitEvent(Event{
			Type:    EvtIssueState,
			IssueID: id,
			Payload: map[string]any{"from": from, "to": target, "reason": tracker.ReasonUnblocked},
		}); err != nil {
			return err
		}
	}
	return nil
}

// lockedIssueGetter implements IssueGetter using the store's index without
// taking the mutex — only safe while the caller already holds s.mu.
type lockedIssueGetter struct{ s *Store }

func (g lockedIssueGetter) Get(id string) (*Issue, error) {
	iss, ok := g.s.index[id]
	if !ok || iss == nil {
		return nil, tracker.ErrNotFound
	}
	return cloneIssue(iss), nil
}

// Delete removes the issue file and emits an issue_deleted event.
func (s *Store) Delete(id string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.recoverMutator("Delete", &err)
	if _, ok := s.index[id]; !ok {
		return tracker.ErrNotFound
	}
	if err := os.Remove(s.issuePath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("native store: remove issue: %w", err)
	}
	delete(s.index, id)
	return s.emitPostCommitEvent(Event{Type: EvtIssueDeleted, IssueID: id})
}

// Resolve returns the full issue ID matching the given prefix. The
// prefix may be the bare UUID (without the "native:" scheme) or the
// full ID. Returns tracker.ErrNotFound if no issue matches and a
// distinct error if multiple match. Walks the in-memory index, so
// O(N) over distinct issues with no filesystem I/O.
func (s *Store) Resolve(prefix string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := prefix
	if !strings.HasPrefix(prefix, "native:") {
		want = "native:" + prefix
	}
	var matches []string
	for id := range s.index {
		if id == want || strings.HasPrefix(id, want) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", tracker.ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("native store: ambiguous prefix %q matches %d issues", prefix, len(matches))
	}
}

func (s *Store) writeIssueLocked(iss *Issue) error {
	if err := validateIssueID(iss.ID); err != nil {
		return err
	}
	expireGiveUp(iss)
	if err := os.MkdirAll(filepath.Join(s.root, issuesDir), dirPerm); err != nil {
		return err
	}
	data, err := json.MarshalIndent(iss, "", "  ")
	if err != nil {
		return fmt.Errorf("native store: marshal issue: %w", err)
	}
	p := s.issuePath(iss.ID)
	if err := store.WriteFileAtomic(p, data, filePerm); err != nil {
		return fmt.Errorf("native store: write issue: %w", err)
	}
	return nil
}

// StateChangePayload builds the state-change event body, stamping the
// provenance when the WRITER is a watchdog. byMarker is the writer's own
// claim marker — the token a fenced write presented — never the marker
// the card happens to carry: an operator moving a card the watchdog is
// conserving is still an operator gesture. Downstream consumers launch
// bots and spend one-shot label gates on these events, and a machine
// repairing a dead owner is not the operator gesture they are written
// for. Exported for the Mongo twin, which builds the same event from
// its own CAS.
func StateChangePayload(from, to, byMarker string) map[string]any {
	p := map[string]any{"from": from, "to": to}
	if tracker.IsReaperMarker(byMarker) {
		p["reason"] = tracker.ReasonWatchdog
	}
	return p
}

// expireGiveUp drops a give-up stamp that no longer describes the state the
// issue is being written in. It runs on the ONE write path rather than at each
// of the several state writers (SetState, ClaimForLaunch, the unblock promote,
// a schema migration) so no future state writer has to remember it — the
// property the stamp's contract claims (Issue.GaveUp) is enforced here rather
// than trusted.
//
// It is what makes staleness PERMANENT. Comparing state at read time alone is
// reversible: a ticket that leaves the stamped state and comes back — with the
// same run, which is the norm since a dispatcher retry resumes the same run id
// — would make the stamp live again and re-file an operator's own decision as
// an unattended give-up, the exact mirror of the bug the stamp exists to fix.
// Once the ticket moves, the stamp is gone for good.
//
// The one thing it cannot catch is a filing that does NOT change the state
// (Close on a ticket already sitting in the give-up's state); those surfaces
// clear the stamp explicitly.
func expireGiveUp(iss *Issue) {
	if iss.GaveUp != nil && iss.GaveUp.State != iss.State {
		iss.GaveUp = nil
	}
}

// readIssueLocked returns a defensive copy of the indexed issue.
// Reads after init always hit the in-memory cache; the on-disk files
// stay authoritative for crash recovery via populateIndex at NewStore.
func (s *Store) readIssueLocked(id string) (*Issue, error) {
	if iss, ok := s.index[id]; ok {
		return cloneIssue(iss), nil
	}
	return nil, tracker.ErrNotFound
}

func (s *Store) issuePath(id string) string {
	return filepath.Join(s.root, issuesDir, encodeID(id)+".json")
}

func validateIssueID(id string) error {
	raw, ok := strings.CutPrefix(id, "native:")
	if !ok || raw == "" {
		return fmt.Errorf("native store: invalid issue id %q", id)
	}
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed.String() != raw {
		return fmt.Errorf("native store: invalid issue id %q", id)
	}
	return nil
}

// Colon is illegal in NTFS filenames; encode "native:<uuid>" → "native__<uuid>"
// for safe cross-platform storage. UUIDs never contain a literal "__".
func encodeID(id string) string { return strings.ReplaceAll(id, ":", "__") }
func decodeID(s string) string  { return strings.ReplaceAll(s, "__", ":") }
