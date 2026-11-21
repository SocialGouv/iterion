package runview

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// This file holds the alert registration seam: every run's observation is
// captured and registered at the point the run starts being observed, and
// released at its episode's end. The registry and the manager's lane are
// the origin-bound alert delivery (R54 slice 3).

// (The seam's serialization lives on Service.alertSeamMu: two services —
// a hot-swap pair — must not serialize each other's seams.)

// registerRunAlertOrigin captures runID's observation once and adopts it as
// the episode's registration: the registry hands out the epoch's lane, the
// manager owns it from adoption to release. The returned release pairs with
// the seam for ABORTED starts only (rollback before the run's goroutine
// takes over); a run's normal termination releases through
// closeRunAndReleaseAlerts. When the manager already holds the observation
// live (a pause/resume), the running epoch is kept and the returned release
// closes THIS caller's own bail — one bail per holder, exactly one release
// each (H5): never a no-op, and never the newest bail by run ID.
func (s *Service) registerRunAlertOrigin(ctx context.Context, runID string) (func(), error) {
	if s.alertRegistry == nil || s.alertManager == nil {
		// No registry (a wrapper store, alerts off): the run keeps today's
		// legacy-only alert path — documented non-uplift, not a fallback.
		return func() {}, nil
	}
	s.alertSeamMu.Lock()
	defer s.alertSeamMu.Unlock()

	obs, err := CaptureRunOriginIn(ctx, s.store, runID)
	if err != nil {
		return nil, err
	}
	// Every holder of the episode takes its own bail (H5): the tailer's
	// ensure, a launch, a detached runner, a reattach each acquire on the
	// live epoch, and the epoch ends with the last holder's release.
	lane := s.alertRegistry.acquire(runID, obs)
	bail, err := s.alertManager.Register(runID, obs, lane)
	if err != nil {
		// Registration refused: the fresh handle stays the caller's (F8).
		_ = lane.Close()
		return nil, err
	}
	// The returned release closes THIS holder's bail — never "the newest
	// bail of the run ID", which a late holder would otherwise take from a
	// successor's episode (H5). Idempotent.
	return func() { bail.Close() }, nil
}

// fetchAlertLane returns the merge's registry lane for its run, adopting
// the registration that matches the merge's binding. wait=true covers the
// Active-before-Register race (F4): the fetch waits (bounded by ctx) for
// the acquire/release broadcast, then fetches once more. wait=false is the
// merge's one-shot re-arm: it never stalls the merge's select loop.
func (s *Service) observedAlertLane(ctx context.Context, runID string, wait bool) (*EventSubscription, store.RunObservation) {
	if s.alertRegistry == nil || s.alertManager == nil {
		return nil, store.RunObservation{}
	}
	// The registration wait is BOUNDED (H2): a seam whose capture failed
	// degraded to a warning, leaving the run Active without a registration
	// and without a broadcast — an unbounded wait would park the stream
	// until its context died, even outliving the run. Past the bound the
	// fetch falls back to the legacy carrier; the merge's lazy lane
	// adoption catches a registration that lands late.
	waitDeadline := time.Now().Add(2 * time.Second)
	for {
		obs, registered := s.alertManager.Observation(runID)
		if registered {
			// Only a bound merge adopts the registry lane, and only the
			// registration its binding names (M14): an unbound merge
			// follows the ID across epochs and keeps the legacy carrier
			// (fact 13); a successor's registration is not this lane.
			if binding, bound := store.RunDataBindingFrom(ctx); !bound || binding.Header() != obs.Header() {
				return nil, store.RunObservation{}
			}
			if sub, ok := s.alertRegistry.subscribe(obs); ok {
				return sub, obs
			}
		}
		if !wait || !s.Active(runID) || ctx.Err() != nil || !time.Now().Before(waitDeadline) {
			return nil, store.RunObservation{}
		}
		// The launch published Active before its registration: wait for the
		// acquire or release broadcast, then fetch once more.
		w := s.alertRegistry.waitSlot(runID)
		if _, registered := s.alertManager.Observation(runID); registered {
			s.alertRegistry.dropSlot(runID, w)
			continue
		}
		select {
		case <-w.ch:
			s.alertRegistry.dropSlot(runID, w)
		case <-ctx.Done():
			s.alertRegistry.dropSlot(runID, w)
			return nil, store.RunObservation{}
		case <-time.After(time.Until(waitDeadline)):
			s.alertRegistry.dropSlot(runID, w)
			return nil, store.RunObservation{}
		}
	}
}
