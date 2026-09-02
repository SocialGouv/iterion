package native

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"strings"
)

// IssueGetter is the minimal lookup surface hard-blocker evaluation needs.
// *Store and every BoardStore implementation satisfy it via Get.
type IssueGetter interface {
	Get(id string) (*Issue, error)
}

// BlockerInfo is a resolved dependency for projection and API responses.
type BlockerInfo struct {
	ID        string   `json:"id"`
	Title     string   `json:"title,omitempty"`
	State     string   `json:"state,omitempty"`
	Bot       string   `json:"bot,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Satisfied bool     `json:"satisfied"`
	// MissingLabels lists required labels still absent when state is done
	// but bot_args.require_blocker_labels is set on the dependent.
	MissingLabels []string `json:"missing_labels,omitempty"`
}

// BlockerPolicy optional gates beyond state==done (V3.2 artefact acceptance).
type BlockerPolicy struct {
	// RequireLabels must all be present on each blocker once it is done.
	RequireLabels []string
}

// BlockingInfo is one reverse-index entry: an issue that lists this one as
// a blocker. Computed on read (V1 — not stored).
type BlockingInfo struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// BlockerSatisfied reports whether one issue satisfies a hard dependency.
// Product rule (multi-pipeline / Town): only StateDone counts. Terminal
// non-success states (e.g. blocked = "won't do") must NOT unblock
// dependents — that was the gap that made StateBlocked unusable as a
// temporary hold.
func BlockerSatisfied(iss *Issue) bool {
	return iss != nil && iss.State == StateDone
}

// NormalizeBlockers trims, drops empties, and dedupes while preserving order.
func NormalizeBlockers(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// ResolveBlockers enriches blocker IDs with the default (state-only) policy.
func ResolveBlockers(g IssueGetter, ids []string) []BlockerInfo {
	return ResolveBlockersPolicy(g, ids, BlockerPolicy{})
}

// ResolveBlockersPolicy enriches blocker IDs. Missing IDs are unsatisfied
// (fail closed). When policy.RequireLabels is set, a done blocker still
// fails until it carries every required label.
func ResolveBlockersPolicy(g IssueGetter, ids []string, policy BlockerPolicy) []BlockerInfo {
	ids = NormalizeBlockers(ids)
	if len(ids) == 0 {
		return nil
	}
	out := make([]BlockerInfo, 0, len(ids))
	for _, id := range ids {
		info := BlockerInfo{ID: id}
		iss, err := g.Get(id)
		if err != nil || iss == nil {
			out = append(out, info)
			continue
		}
		info.Title = iss.Title
		info.State = iss.State
		info.Bot = iss.Bot
		if len(iss.Labels) > 0 {
			info.Labels = append([]string(nil), iss.Labels...)
		}
		info.Satisfied = BlockerSatisfied(iss)
		if info.Satisfied && len(policy.RequireLabels) > 0 {
			var missing []string
			have := make(map[string]struct{}, len(iss.Labels))
			for _, l := range iss.Labels {
				have[l] = struct{}{}
			}
			for _, want := range policy.RequireLabels {
				if _, ok := have[want]; !ok {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				info.Satisfied = false
				info.MissingLabels = missing
			}
		}
		out = append(out, info)
	}
	return out
}

// BlockersSatisfied returns ok when every blocker is StateDone (no label policy).
func BlockersSatisfied(g IssueGetter, ids []string) (ok bool, open []BlockerInfo) {
	return BlockersSatisfiedPolicy(g, ids, BlockerPolicy{})
}

// BlockersSatisfiedPolicy returns ok when every blocker passes state+labels.
func BlockersSatisfiedPolicy(g IssueGetter, ids []string, policy BlockerPolicy) (ok bool, open []BlockerInfo) {
	resolved := ResolveBlockersPolicy(g, ids, policy)
	if len(resolved) == 0 {
		return true, nil
	}
	for _, b := range resolved {
		if !b.Satisfied {
			open = append(open, b)
		}
	}
	return len(open) == 0, open
}

// BlockersSatisfiedForIssue applies the dependent's bot_args policy
// (require_blocker_labels) to its blockers list.
func BlockersSatisfiedForIssue(g IssueGetter, iss *Issue) (ok bool, open []BlockerInfo) {
	if iss == nil {
		return true, nil
	}
	policy := BlockerPolicy{RequireLabels: RequireBlockerLabels(iss.BotArgs)}
	return BlockersSatisfiedPolicy(g, iss.Blockers, policy)
}

// ResolveBlockersForIssue is the projection counterpart of BlockersSatisfiedForIssue.
func ResolveBlockersForIssue(g IssueGetter, iss *Issue) []BlockerInfo {
	if iss == nil {
		return nil
	}
	policy := BlockerPolicy{RequireLabels: RequireBlockerLabels(iss.BotArgs)}
	return ResolveBlockersPolicy(g, iss.Blockers, policy)
}

// OpenBlockerCount is the number of unsatisfied hard blockers (state-only).
func OpenBlockerCount(g IssueGetter, ids []string) int {
	_, open := BlockersSatisfied(g, ids)
	return len(open)
}

// CanLaunch is the unified admission rule for a pipeline / dispatcher ticket:
//
//	has a non-empty bot
//	AND state is StateReady (the only launch-eligible staging state for /pipelines)
//	AND every hard blocker is StateDone
//	AND optional require_blocker_labels on those blockers are present
//
// The studio launch loop and the dispatcher adapter share this helper so a
// ticket cannot slip through one path while being gated by the other.
// Run-store freshness (already-finished last run, etc.) is a separate check
// owned by the admission loop — this is the board-side gate only.
func CanLaunch(g IssueGetter, iss *Issue) bool {
	return LaunchBlockedReason(g, iss) == ""
}

// LaunchBlockedReason returns a short machine reason when CanLaunch would
// refuse, or "" when the board-side gate is clear.
func LaunchBlockedReason(g IssueGetter, iss *Issue) string {
	if iss == nil {
		return "missing_issue"
	}
	if strings.TrimSpace(iss.Bot) == "" {
		return "no_bot"
	}
	if iss.State == StateWaitingDeps {
		return "waiting_deps"
	}
	if iss.State != StateReady {
		return "not_ready"
	}
	ok, open := BlockersSatisfiedForIssue(g, iss)
	if !ok {
		for _, b := range open {
			if len(b.MissingLabels) > 0 {
				return "blocker_labels"
			}
		}
		return "open_blockers"
	}
	return ""
}

// ReverseBlockers builds the reverse index for one issue id: every other
// issue that lists it in Blockers. Computed on read (V1).
func ReverseBlockers(all []*Issue, id string) []BlockingInfo {
	if id == "" {
		return nil
	}
	var out []BlockingInfo
	for _, iss := range all {
		if iss == nil {
			continue
		}
		for _, b := range iss.Blockers {
			if b == id {
				out = append(out, BlockingInfo{ID: iss.ID, Title: iss.Title})
				break
			}
		}
	}
	return out
}

// WouldCreateCycle reports whether setting issue id's blockers to the given
// list would introduce a cycle (A→B→A). Missing blocker IDs cannot form a
// path and are ignored; a self-reference is always a cycle.
func WouldCreateCycle(g IssueGetter, id string, blockers []string) bool {
	if id == "" {
		return false
	}
	for _, b := range NormalizeBlockers(blockers) {
		if b == id {
			return true
		}
		if reaches(g, b, id, map[string]bool{}) {
			return true
		}
	}
	return false
}

func reaches(g IssueGetter, from, target string, visiting map[string]bool) bool {
	if from == target {
		return true
	}
	if visiting[from] {
		return false
	}
	visiting[from] = true
	iss, err := g.Get(from)
	if err != nil || iss == nil {
		return false
	}
	for _, b := range iss.Blockers {
		if reaches(g, strings.TrimSpace(b), target, visiting) {
			return true
		}
	}
	return false
}

// ValidateBlockers returns an error when the proposed blockers list would
// create a cycle for issue id. Missing IDs are allowed (fail-closed at
// launch); only cycles are rejected at write time.
func ValidateBlockers(g IssueGetter, id string, blockers []string) error {
	if WouldCreateCycle(g, id, blockers) {
		return fmt.Errorf("blockers: cycle detected")
	}
	return nil
}

// AutoReadyFromArgs reports whether bot_args request auto-promotion to Ready
// when the last hard blocker becomes done.
func AutoReadyFromArgs(args map[string]string) bool {
	if args == nil {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(args["auto_ready"]))
	return v == "true" || v == "1" || v == "yes"
}

// UnblockTarget returns the state a waiting_deps ticket should move to when
// its last hard blocker becomes done. Default: backlog (human decides Ready);
// opt-in StateReady when bot_args.auto_ready is truthy. Empty when the board
// lacks the preferred target (caller keeps the ticket put).
func UnblockTarget(board *Board, iss *Issue) string {
	if board == nil || iss == nil {
		return ""
	}
	want := StateBacklog
	if AutoReadyFromArgs(iss.BotArgs) {
		want = StateReady
	}
	if board.StateByName(want) != nil {
		return want
	}
	// Fallbacks: ready if backlog missing, else nothing.
	if want != StateReady && board.StateByName(StateReady) != nil {
		return StateReady
	}
	return ""
}

// PromoteUnblockedDependents is the BoardStore-facing auto-promote used after
// a ticket reaches StateDone (Mongo store, or any backend without an
// in-mutex index). Dependents in StateWaitingDeps whose hard blockers are
// now all satisfied move to UnblockTarget. Best-effort: a failed promote
// does not roll back the closed ticket. The filesystem *Store uses a
// locked sibling inside SetState instead of this helper to avoid re-lock.
func PromoteUnblockedDependents(store BoardStore, closedID string) error {
	if store == nil || closedID == "" {
		return nil
	}
	board := store.Board()
	if board == nil || board.StateByName(StateWaitingDeps) == nil {
		return nil
	}
	candidates, err := store.List(ListFilter{States: []string{StateWaitingDeps}})
	if err != nil {
		return err
	}
	for _, iss := range candidates {
		if iss == nil {
			continue
		}
		lists := false
		for _, b := range iss.Blockers {
			if b == closedID {
				lists = true
				break
			}
		}
		if !lists {
			continue
		}
		ok, _ := BlockersSatisfiedForIssue(store, iss)
		if !ok {
			continue
		}
		target := UnblockTarget(board, iss)
		if target == "" || target == iss.State {
			continue
		}
		// SetState may re-enter Promote on done only — target is backlog/ready.
		// Prefer the reason-carrying setter: the promote is a CASCADE and its
		// provenance must reach the spine on this twin like on the FS one.
		if rs, ok := store.(StateReasoner); ok {
			if _, err := rs.SetStateWithReason(iss.ID, target, tracker.ReasonUnblocked); err != nil {
				return err
			}
		} else if _, err := store.SetState(iss.ID, target); err != nil {
			return err
		}
		// Note: issue_unblocked is emitted by the filesystem store's locked
		// path; for BoardStore backends that only emit issue_state_changed,
		// that event is still enough for audit tails. Mongo emit of
		// EvtIssueUnblocked can be added when a second consumer needs it.
	}
	return nil
}
