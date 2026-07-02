package runview

import "sync"

// fileSrcHandle tracks a refcounted events.jsonl tailer started on
// demand for a run that is NOT produced in this process — e.g. a
// dispatcher-spawned in-process run, whose runtime observer bridges
// events to the dispatcher heartbeat (pkg/dispatcher/engine_runner.go)
// rather than to this service's broker. Multiple WS subscribers to the
// same run share one tailer; it stops when the last one releases.
type fileSrcHandle struct {
	refs int
	done chan struct{}
}

// ensureEventSource guarantees a file-backed event tailer is feeding
// the broker for runID, bridging events.jsonl -> broker so WS
// subscribers receive LIVE events for runs this process didn't launch
// in-process. Returns a release func the caller MUST invoke exactly
// once when its subscription ends; the tailer is stopped when the last
// holder releases.
//
// Only call this for runs whose in-process runtime observer does NOT
// already publish to the broker — i.e. guard with !s.Active(runID) in
// local same-store mode. For in-process Launch runs the broker is fed
// directly (service_launch.go), so a file tailer would double-publish
// (harmless for the seq-dedup'd WS layer, but wasteful).
func (s *Service) ensureEventSource(runID string) (release func()) {
	s.fileSrcMu.Lock()
	if s.fileSrcs == nil {
		s.fileSrcs = make(map[string]*fileSrcHandle)
	}
	h := s.fileSrcs[runID]
	if h == nil {
		h = &fileSrcHandle{done: make(chan struct{})}
		s.fileSrcs[runID] = h
		startEventSource(s, runID, h.done)
	}
	h.refs++
	s.fileSrcMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.fileSrcMu.Lock()
			defer s.fileSrcMu.Unlock()
			cur := s.fileSrcs[runID]
			if cur == nil {
				return
			}
			cur.refs--
			if cur.refs <= 0 {
				close(cur.done)
				delete(s.fileSrcs, runID)
			}
		})
	}
}

// Active reports whether runID is being produced in-process by this
// service's lifecycle manager (a studio / CLI Launch). Dispatcher-
// spawned runs are not registered with the manager and return false —
// which is exactly the signal ensureEventSource keys off.
func (s *Service) Active(runID string) bool {
	return s.manager != nil && s.manager.Active(runID)
}
