package runview

import (
	"context"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// startEventSource spawns a goroutine that tails the run's events.jsonl
// (via the shared runstream tailer) and republishes each appended line
// through the service's EventBroker, so WS subscribers connected to the
// studio server receive events emitted by a detached runner subprocess,
// an external `iterion run`, or a dispatcher-spawned run.
//
// The tailer terminates when the supplied done channel is closed —
// which spawnDetached arranges to happen when the runner subprocess
// exits, and the refcounted ensure-source release arranges for external
// runs when the last WS subscriber detaches.
func startEventSource(s *Service, runID string, done <-chan struct{}) {
	path := filepath.Join(s.storeDir, "runs", runID, "events.jsonl")
	emit := func(evt store.Event) {
		s.broker.Publish(evt)
		if s.alertManager != nil {
			s.alertManager.Observe(evt)
		}
	}
	// The rotation is the one observable handover of the ID-keyed path
	// (F2): a replacement of the run under the same ID resets the tailer's
	// offset, and the re-registration must fire before the new file's
	// first line reaches Observe. The capture is team-blind: the tailer
	// has no caller context.
	// The rotation's own bail is kept and released here: every acquire has
	// exactly one owner release (H5/codex-F1), and a re-registration that
	// returned without keeping its bail would leak the epoch.
	var rotateRelease func()
	onRotate := func() {
		if rotateRelease != nil {
			rotateRelease()
		}
		release, err := s.registerRunAlertOrigin(store.TeamBlind(context.Background()), runID)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("runview: alert re-registration of %s at rotation: %v", runID, err)
			}
			return
		}
		rotateRelease = release
	}
	go func() {
		runstream.TailEventsFile(path, done, emit, s.logger, onRotate)
		if rotateRelease != nil {
			rotateRelease()
		}
	}()
}

// startObservedEventSource starts the ID-keyed events tailer AND the run's
// alert registration as one step: the registration precedes any event of
// that origin (capture ordering, §7). It replaces the bare startEventSource
// calls of the detached launch/resume and reattach paths, whose
// terminations release through closeRunAndReleaseAlerts. A registration
// failure degrades to a warning — alerts stay off for the run, its event
// bridging is unaffected.
func startObservedEventSource(s *Service, runID string, done <-chan struct{}, ctx context.Context) {
	release, err := s.registerRunAlertOrigin(ctx, runID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("runview: alert registration of %s: %v — alerts stay off for this run", runID, err)
		}
	}
	startEventSource(s, runID, done)
	// The producer's bail ends with its tailer — including a tailer stopped
	// by its last subscriber, never by the run's own terminal (that pairing
	// is closeRunAndReleaseAlerts, and it is idempotent with this one).
	if release != nil {
		go func() { <-done; release() }()
	}
}

// closeRunAndReleaseAlerts is the termination pairing of every producer
// path (the spawnRun defer, a detached runner's exit, a reattached
// runner's exit): the broker forgets the run ID and the alert
// registration's epoch ends with it.
func (s *Service) closeRunAndReleaseAlerts(runID string) {
	s.broker.CloseRun(runID)
	if s.alertManager != nil {
		// The run is over: every bail still held for it closes — no holder
		// outlives its run (a paused producer's bail included).
		s.alertManager.ReleaseRun(runID)
	}
}
