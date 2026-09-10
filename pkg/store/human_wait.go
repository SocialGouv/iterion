package store

import (
	"context"
	"time"
)

// maxParkedProbeRuns bounds the cost of one persisted wait probe. If no proof
// is found within this budget, stall detection remains armed.
const maxParkedProbeRuns = 256

// HasBlockingHumanWait reports persisted proof of an active await_answers
// sync point or a paused subbot descendant. Pending async questions alone
// never qualify. An expired sync-point marker never qualifies either.
//
// It is the stall watchdog's exemption oracle, and it is a PULL from persisted
// truth rather than a heartbeat the parked code pushes: a parent blocked in
// runview.AwaitSubbotTerminal emits no event and books no work, so any signal
// it produced itself would either be invisible to the dispatcher or would have
// to fake node progress on the run's timeline. What it does leave behind is
// exact — the child id under SubbotChildren (written before the child engine
// runs) and the child's own paused status — so the watchdog reads that instead.
//
// Answering a paused descendant or leaving the sync point removes its proof.
// Sync-point proof also expires at the node's mandatory timeout, so a crashed
// worker cannot leave an indefinite exemption. Both the dispatcher and alert
// manager use this predicate; no synthetic progress event is needed.
//
// Fails CLOSED. An unreadable record, a pruned child, a cycle in a corrupt
// SubbotChildren map, or a probe past its budget all read as "not parked": no
// information is not a licence to disarm the watchdog.
func HasBlockingHumanWait(ctx context.Context, rs RunStore, runID string, now time.Time) bool {
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
			// The sync point itself (including a root) may wait silently, but
			// merely having async questions outstanding is never evidence.
			if run.Status == RunStatusRunning {
				for _, wait := range run.AwaitAnswersWaits {
					if wait.NodeID != "" && !wait.Until.IsZero() && now.Before(wait.Until) {
						return true
					}
				}
			}
			if run.Status.IsTerminal() {
				continue
			}
			if !root {
				// IsPaused is the same predicate finishRun's pause arm reads,
				// so "what parks a card" and "what exempts a parent" cannot
				// drift apart.
				if run.Status.IsPaused() {
					return true
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
