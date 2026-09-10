package dispatcher

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Keep the dispatcher seam while sharing the persisted oracle with alerts.
func parkedOnPausedDescendant(ctx context.Context, rs store.RunStore, runID string) bool {
	return store.HasBlockingHumanWait(ctx, rs, runID, time.Now())
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
			"dispatcher: %s is silent because run %s or a subbot descendant awaits human input — exempt from the stall watchdog until it is answered",
			r.Identifier, r.RunID,
		)
	case !parked && r.parkedNoticed:
		r.parkedNoticed = false
		c.logger.Info(
			"dispatcher: %s no longer has a recorded human wait (run=%s) — the stall watchdog applies again",
			r.Identifier, r.RunID,
		)
	}
	return parked
}
