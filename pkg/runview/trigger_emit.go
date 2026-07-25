package runview

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// emitRunOutcome publishes a run-outcome trigger.Event when a run leaves the
// engine — the "runned by iterion" source that lets a finished/failed run fire
// downstream runs (pipelines, on-failure escalation) and a paused run mark its
// board card "awaiting input". It is a best-effort no-op unless an event
// publisher is wired, and never blocks the run goroutine on a slow consumer
// (the bus fan-out is itself lossy). The event construction (kind derivation,
// tenant/owner enrichment, per-episode ID) lives in trigger.BuildRunOutcome,
// shared with the cloud runner's publish hook.
func (s *Service) emitRunOutcome(runID string, bodyErr error) {
	if s.eventPublisher == nil {
		return
	}
	fctx := store.WithoutTenantFilter(context.Background())
	ev := trigger.BuildRunOutcome(fctx, s.store, runID, bodyErr)
	if err := s.eventPublisher.Publish(fctx, ev); err != nil && s.logger != nil {
		s.logger.Error("runview: publish %s trigger event for run %s: %v — chained subscriptions will not fire", ev.Kind, runID, err)
	}
}
