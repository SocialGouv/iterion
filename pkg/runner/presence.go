package runner

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
)

// FleetPresence is the runner's record in the fleet registry
// (natsq.Presence): what the engine floor reads to learn which builds can
// execute a new launch.
type FleetPresence interface {
	// Fresh reports whether the record is known to be in the registry. A
	// runner the registry cannot see must not take work: the floor would
	// not count its build.
	Fresh() bool
	// MarkDraining records that the runner has stopped fetching.
	MarkDraining(ctx context.Context) error
}

// presencePauseDelay spaces the freshness checks of a loop paused on a
// stale record.
var presencePauseDelay = 2 * time.Second

// presenceMarkTimeout bounds the draining write in Shutdown.
const presenceMarkTimeout = 5 * time.Second

// presenceAllowsFetch is the fetch gate: true without a registry (local
// runs), else true only while this runner's record is fresh. The pause is
// published (presencePaused) for the probes and the stall detector, and
// the pause and the resume each log once.
func (r *Runner) presenceAllowsFetch() bool {
	if r.cfg.Presence == nil {
		return true
	}
	fresh := r.cfg.Presence.Fresh()
	wasPaused := r.presencePaused.Swap(!fresh)
	switch {
	case !fresh && !wasPaused:
		r.cfg.Logger.Warn("runner: the fleet registry has not stored this runner's record recently — fetching paused until a heartbeat lands (an unseen runner must not take work the engine floor did not count)")
	case fresh && wasPaused:
		r.cfg.Logger.Info("runner: fleet registry record fresh again — fetching resumes")
	}
	return fresh
}

// markDraining records, off the shutdown path, that the runner has stopped
// fetching. Shutdown calls it only after cancelling the fetch loop, and
// never waits on it: a slow registry must not delay the drain it reports.
// The record keeps the mark once set, so a failed write is retried by the
// next heartbeat; until one lands the floor still counts this runner — the
// conservative side.
func (r *Runner) markDraining() {
	if r.cfg.Presence == nil {
		return
	}
	errtrack.Go("runner.markDraining", func() {
		ctx, cancel := context.WithTimeout(context.Background(), presenceMarkTimeout)
		defer cancel()
		if err := r.cfg.Presence.MarkDraining(ctx); err != nil {
			r.cfg.Logger.Warn("runner: %v — the next heartbeat carries the draining mark; until one lands the engine floor still counts this runner", err)
		}
	})
}
