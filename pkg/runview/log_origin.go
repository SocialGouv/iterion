package runview

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// This file holds the log lane seam: every producer that writes a run's
// run.log registers that run's observation and adopts the epoch's lane, and a
// bound subscriber adopts the lane its binding names. The lane carries wakes
// and the epoch's end only; the bytes are the store's (R54 slice 4).

// logLaneWaitBound bounds the first registration wait of a bound subscriber
// (H2/I6): a stream is never parked indefinitely on a registration that will
// not come.
var logLaneWaitBound = 2 * time.Second

// runLogPath is the canonical run.log of runID under the store's root. It
// reports false when the store has no filesystem: a memory-only store has no
// log bytes to bind to.
func (s *Service) runLogPath(runID string) (string, bool) {
	if s == nil {
		return "", false
	}
	root := s.StoreRoot()
	if root == "" {
		return "", false
	}
	return filepath.Join(root, "runs", runID, "run.log"), true
}

// logDurableAtStore reports whether the store persists run logs at all. It is
// a capacity guard, not a per-run answer: a run whose tee failed is
// logMemoryOnly and is refused at adoption.
func logDurableAtStore(st store.RunStore) bool {
	return store.AsRunLogStore(st) != nil
}

// detachedLogDurability is a detached producer's durability: the runner
// subprocess writes run.log itself, under the store's root, so the bytes
// reach the store whenever that root exists — the reattaching process' buffer
// has no file tee of its own.
func (s *Service) detachedLogDurability() logDurability {
	if s == nil || s.StoreRoot() == "" {
		return logMemoryOnly
	}
	return logDurable
}

// classifyLogExit is the ONE table an abandoned producer is classified by:
// a paused run keeps its subscribers for the resume, a terminal run ends the
// stream, and anything else is lost — a typed error at the subscriber, never
// a silent completion.
func classifyLogExit(status store.RunStatus) logEnd {
	switch {
	case status.IsPaused():
		return logEndPaused
	case status.IsTerminal():
		return logEndTerminal
	default:
		return logEndLost
	}
}

// registerRunLogOrigin captures runID's observation and adopts it as an epoch
// of the log registry, attaching the buffer's wake hook. It returns nil — and
// no lane — when the store persists no run logs, when the process has no
// registry, or when the capture fails; a capture failure is RECORDED as the
// run's typed end (a retained producer failure), never degraded to a settle.
func (s *Service) registerRunLogOrigin(ctx context.Context, runID string, buf *RunLogBuffer, dur logDurability) *logLane {
	if s == nil || s.logRegistry == nil || buf == nil {
		return nil
	}
	buf.markLaneHandled()
	if !logDurableAtStore(s.store) {
		if s.logger != nil {
			s.logger.Warn("runview: run %s: the store persists no run logs — log lanes stay off", runID)
		}
		return nil
	}
	s.logSeamMu.Lock()
	defer s.logSeamMu.Unlock()

	obs, err := CaptureRunOriginIn(ctx, s.store, runID)
	if err != nil {
		s.recordLogProducerFailure(runID, fmt.Errorf("runview: capture the log origin of %s: %w", runID, err))
		return nil
	}
	lane := s.logRegistry.acquire(runID, obs, dur)
	// The single typing site of a tee failure: the chain carries both the
	// missing capability and the OS cause, so the stream fails as a permanent
	// error instead of a silent truncation.
	buf.attachLogLane(lane, func(teeErr error) {
		// The producer already registered: a tee failure after its epoch ended
		// is late and ignored, never a second end for the same generation.
		s.recordLogTeeFailure(runID, fmt.Errorf("runview: run %s log tee: %w", runID, errors.Join(store.ErrRunDataCapability, teeErr)))
	})
	return lane
}

// observedLogLane returns the merge's lane for runID, adopting the CURRENT
// epoch — open or retained-closed — that its binding names. wait=true covers
// the launch-before-registration race (the launch may register its lane just
// after the subscription), bounded by logLaneWaitBound; wait=false is the
// merge's one-shot re-arm and never stalls.
//
// It returns a nil subscription when the caller is unbound (the legacy
// carrier serves it) or when no epoch lands in the window. A lane of another
// header or a memory-only lane is a typed refusal: adoption checks identity
// and durability at EVERY adoption, not only the first.
func (s *Service) observedLogLane(ctx context.Context, runID string, wait bool) (*LogWakeSubscription, store.RunObservation, error) {
	if s == nil || s.logRegistry == nil {
		return nil, store.RunObservation{}, nil
	}
	binding, bound := store.RunDataBindingFrom(ctx)
	if !bound {
		return nil, store.RunObservation{}, nil
	}
	deadline := time.Now().Add(logLaneWaitBound)
	for {
		if obs, _, ok := s.logRegistry.currentFor(runID); ok {
			if binding.Header() != obs.Header() {
				return nil, obs, fmt.Errorf("runview: log lane of %s does not belong to the bound birth: %w", runID, store.ErrRunDataIdentityChanged)
			}
			// A retained end belongs to the epoch before the one starting now:
			// when a later failure took that epoch over, adopting its value
			// would announce a clean end for a run whose producer failed.
			if s.logRegistry.supersededByFailure(runID) {
				if _, ferr, ok := s.logRegistry.failureFor(runID); ok {
					return nil, obs, fmt.Errorf("runview: log lane of %s: the current producer failed: %w", runID, ferr)
				}
			}
			if sub, ok := s.logRegistry.subscribe(obs); ok {
				if err := s.admitLogLane(sub, binding); err != nil {
					sub.Cancel()
					return nil, obs, err
				}
				return sub, obs, nil
			}
		}
		// The bounded wait covers the launch-before-registration race, which
		// only exists for a run this process is driving: a finished or
		// external run has no producer here to register, and the merge's
		// watch adopts one that lands later anyway (same rule as the alert
		// lane, alert_origin.go:86).
		if !wait || !s.Active(runID) || ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, store.RunObservation{}, nil
		}
		w := s.logRegistry.waitSlot(runID)
		if _, _, ok := s.logRegistry.currentFor(runID); ok {
			s.logRegistry.dropSlot(runID, w)
			continue
		}
		select {
		case <-w.ch:
			s.logRegistry.dropSlot(runID, w)
		case <-ctx.Done():
			s.logRegistry.dropSlot(runID, w)
			return nil, store.RunObservation{}, nil
		case <-time.After(time.Until(deadline)):
			s.logRegistry.dropSlot(runID, w)
			return nil, store.RunObservation{}, nil
		}
	}
}

// admitLogLane checks a subscription against the reader's binding: the lane
// must be the bound birth's, and its producer must have reached the store.
func (s *Service) admitLogLane(sub *LogWakeSubscription, binding store.RunDataBinding) error {
	if sub == nil {
		return nil
	}
	if binding.Header() != sub.obs.Header() {
		return fmt.Errorf("runview: log lane of %s is not the bound birth's: %w", binding.Header().RunID, store.ErrRunDataIdentityChanged)
	}
	if !sub.Durable {
		return fmt.Errorf("runview: log lane of %s has no durable run.log: %w", binding.Header().RunID, store.ErrRunDataCapability)
	}
	return nil
}

// recordLogProducerFailure retains a local producer's failure as the run's
// end and notifies its watch: it is a fin like logEndLost, never a settle.
func (s *Service) recordLogProducerFailure(runID string, err error) {
	if s == nil || s.logRegistry == nil || err == nil {
		return
	}
	if s.logger != nil {
		s.logger.Warn("runview: run %s log producer stopped: %v", runID, err)
	}
	s.logRegistry.recordFailure(runID, err)
}

// logProducerFailureFor is localLogProducerFailure with the observation the
// cause belongs to: a reader bound to another birth refuses a known mismatch,
// because a successor's failed producer is not this stream's end.
func (s *Service) logProducerFailureFor(runID string) (store.RunObservation, error, bool) {
	if s == nil || s.logRegistry == nil {
		return store.RunObservation{}, nil, false
	}
	return s.logRegistry.failureFor(runID)
}

// recordLogTeeFailure is recordLogProducerFailure for a producer that already
// registered: its failure ends the open epoch, and a late one is dropped.
func (s *Service) recordLogTeeFailure(runID string, err error) {
	if s == nil || s.logRegistry == nil || err == nil {
		return
	}
	if s.logger != nil {
		s.logger.Warn("runview: run %s log tee stopped: %v", runID, err)
	}
	s.logRegistry.recordFailureOfOpenEpoch(runID, err)
}

// localLogProducerFailure returns the retained failure of runID's local log
// producer, nil when none was recorded or a fin was applied first.
func (s *Service) localLogProducerFailure(runID string) error {
	if s == nil || s.logRegistry == nil {
		return nil
	}
	err, _ := s.logRegistry.failure(runID)
	return err
}

// clearLogProducerFailure drops the registry-level copy of a failure once the
// run's retained epoch carries it (a buffer teardown). A failure with no
// epoch stays: it must outlive the producer's bail.
func (s *Service) clearLogProducerFailure(runID string) {
	if s == nil || s.logRegistry == nil {
		return
	}
	s.logRegistry.clearFailure(runID)
}
