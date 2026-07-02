package runview

import (
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
	go runstream.TailEventsFile(path, done, emit, s.logger)
}
