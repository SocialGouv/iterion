package dispatcher

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/store"
)

// maxParkedProbeRuns bounds how many run records one parked-descendant probe
// reads. The answer a parked run needs is found at depth 1 (its own children),
// so the budget only bites on a deep or wide tree in which NOTHING is paused —
// exactly the tree that should be reaped. Exceeding it therefore returns "not
// parked", the fail-closed answer.
const maxParkedProbeRuns = 256

// parkedOnPausedDescendant reports whether a silent run is silent BECAUSE a
// run below it is parked on a pause only an operator can lift.
//
// It is the stall watchdog's exemption oracle, and it is a PULL from persisted
// truth rather than a heartbeat the parked code pushes: a parent blocked in
// runview.AwaitSubbotTerminal emits no event and books no work, so any signal
// it produced itself would either be invisible to the dispatcher or would have
// to fake node progress on the run's timeline. What it does leave behind is
// exact — the child id under SubbotChildren (written before the child engine
// runs) and the child's own paused status — so the watchdog reads that instead.
//
// The exemption is self-limiting by construction: the moment the operator
// answers, the descendant leaves its paused status and the next tick judges the
// run on its silence alone. A run that is merely wedged, at any depth, is never
// exempt.
//
// Fails CLOSED. An unreadable record, a pruned child, a cycle in a corrupt
// SubbotChildren map, or a probe past its budget all read as "not parked": no
// information is not a licence to disarm the watchdog.
func parkedOnPausedDescendant(ctx context.Context, rs store.RunStore, runID string) bool {
	if rs == nil || runID == "" {
		return false
	}
	// Breadth-first over SubbotChildren, one store read per run. `seen` is what
	// guarantees termination on a corrupt map that points back at an ancestor;
	// the budget only bounds cost.
	seen := map[string]bool{runID: true}
	frontier := []string{runID}
	budget := maxParkedProbeRuns
	// The ROOT's own pause is deliberately NOT an exemption: a run that pauses
	// itself returns from the engine, and finishRun's pause arm parks the card
	// and frees the slot. Only a descendant keeps a parent silent while the
	// parent is still `running`.
	root := true

	for len(frontier) > 0 && budget > 0 {
		var next []string
		for _, id := range frontier {
			if budget <= 0 {
				break
			}
			run, err := rs.LoadRun(ctx, id)
			budget--
			if err != nil || run == nil {
				continue
			}
			if !root {
				// IsPaused is the same predicate finishRun's pause arm reads,
				// so "what parks a card" and "what exempts a parent" cannot
				// drift apart.
				if run.Status.IsPaused() {
					return true
				}
				// IsTerminal matches runview.AwaitSubbotTerminal's own terminal
				// set: a child in one of those states is something the parent
				// stops waiting on, so its subtree is not worth walking.
				if run.Status.IsTerminal() {
					continue
				}
			}
			for _, childID := range run.SubbotChildren {
				if childID == "" || seen[childID] {
					continue
				}
				seen[childID] = true
				next = append(next, childID)
			}
		}
		frontier = next
		root = false
	}
	return false
}

// exemptParkedFromStall answers reconcileStalled's question for one entry:
// should this aged-out run be spared? It also owns the operator-visible log
// line, bracketed per park episode so a multi-day review costs two lines and
// not one per tick — an exemption nobody can see reads as a hung watchdog.
//
// Actor-goroutine only: it mutates the entry's episode flag and reads the
// store, both under the same ADR-028 Step 3 boundary as the parked sweeps.
func (c *Dispatcher) exemptParkedFromStall(ctx context.Context, rs store.RunStore, r *runningEntry) bool {
	parked := parkedOnPausedDescendant(ctx, rs, r.RunID)
	switch {
	case parked && !r.parkedNoticed:
		r.parkedNoticed = true
		c.logger.Info(
			"dispatcher: %s is silent because a subbot descendant of run %s awaits human input — exempt from the stall watchdog until it is answered",
			r.Identifier, r.RunID,
		)
	case !parked && r.parkedNoticed:
		r.parkedNoticed = false
		c.logger.Info(
			"dispatcher: %s no longer waits on a paused descendant (run=%s) — the stall watchdog applies again",
			r.Identifier, r.RunID,
		)
	}
	return parked
}
