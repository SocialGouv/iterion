package native

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// The terminal-write guard: a Terminal:true state is a SINK. Entering
// one is free from anywhere (closing is always legitimate); LEAVING one
// through the ordinary SetState family is refused — the silent
// resurrection of a done/blocked card was any→any's worst case. The one
// sanctioned exit is Reopen, an operator-surface op with its own audit
// trail; automation never reopens (the reaper files INTO terminals, the
// cascade promotes FROM waiting_deps, launch claims FROM ready — none
// of the machine writers ever legitimately exits a terminal, which is
// what lets the guard live at the shared choke without per-writer
// exemptions).
//
// Direct state writers verified against the guard by their own source
// constraints (the F18 inventory): ClaimForLaunch (StateReady only) and
// PromoteUnblockedDependents (StateWaitingDeps only).
//
// The board column migrations are the ONE writer that can cross the
// boundary: RenameState moves nothing, but DeleteState(terminal,
// migrate_to: <working>) carries a whole column across it. That is an
// explicit operator gesture on the board's own schema, so it is allowed —
// but it is held to Reopen's dependents check (see
// reopenMigrationAllowedLocked), because a bulk reopen must not be a way
// around the refusal a single-card reopen would get.

// ValidateStateExit is the shared gate both twins' SetState family
// calls. from == to never reaches it (no-ops return earlier).
func ValidateStateExit(b *Board, from, to string) error {
	st := b.StateByName(from)
	if st == nil || !st.Terminal {
		return nil
	}
	// A terminal→terminal move is an operator REFILING (acknowledging a
	// give-up by closing a blocked card as done, or the reverse) — it
	// resurrects no work, so it stays an ordinary move. The class the
	// sink kills is terminal→working: silent resurrection.
	if dst := b.StateByName(to); dst != nil && dst.Terminal {
		return nil
	}
	return fmt.Errorf("%w: %q is terminal — leaving it requires an explicit reopen (state %q refused)",
		tracker.ErrTerminalStateExit, from, to)
}

// TerminalStateNames lists the board's sink columns. The Mongo twin
// needs them as a CAS precondition: it cannot check-then-act without
// reopening the TOCTOU the fence closes, so the guard travels INTO the
// conditional write as a `$nin` on the source state.
func TerminalStateNames(b *Board) []string {
	var out []string
	for _, st := range b.States {
		if st.Terminal {
			out = append(out, st.Name)
		}
	}
	return out
}

// ReopenBlockedByDependents lists the dependents whose promotion this
// card's DONE already triggered: reopening under them would un-satisfy
// a dependency their launch already consumed. Deterministic v1: refuse
// with the list; re-blocking dependents atomically is a later
// refinement if the need ever materialises.
// The caller supplies the full listing: the FS store iterates its index
// under the lock it already holds (calling the public List here was a
// self-deadlock), the Mongo store passes its List result.
func ReopenBlockedByDependents(all []*Issue, id, from string) error {
	if from != StateDone {
		return nil // only done satisfies blockers, only done promoted anyone
	}
	return reopenBlocked(promotedDependents(all), id, from)
}

// promotedDependents indexes blocker → the dependents already promoted on
// it. Built ONCE per gesture: the per-card scan it replaces made a bulk
// column migration quadratic, and on the FS twin that ran under the
// store-wide lock (measured: 62ms at 8k cards, ~10s at 100k, with every
// read blocked behind it).
func promotedDependents(all []*Issue) map[string][]string {
	idx := make(map[string][]string)
	for _, iss := range all {
		if iss.State == StateWaitingDeps {
			continue
		}
		for _, b := range iss.Blockers {
			idx[b] = append(idx[b], iss.ID)
		}
	}
	return idx
}

func reopenBlocked(idx map[string][]string, id, from string) error {
	if from != StateDone {
		return nil
	}
	promoted := idx[id]
	if len(promoted) > 0 {
		return fmt.Errorf("%w: reopening %s would un-satisfy dependents already promoted on its completion (%s) — resolve or re-block them first",
			tracker.ErrTransitionRejected, id, strings.Join(promoted, ", "))
	}
	return nil
}

// SetStateOrReopen is the OPERATOR-surface helper: an ordinary move,
// falling back to the sanctioned Reopen when the source is terminal. A
// bot surface (boardops) must NOT use it — a run that can drag a card
// out of done has the exact power the guard removes.
func SetStateOrReopen(s BoardStore, id, to string) (*Issue, error) {
	iss, err := s.SetState(id, to)
	if err != nil && errors.Is(err, tracker.ErrTerminalStateExit) {
		return s.Reopen(id, to)
	}
	return iss, err
}
