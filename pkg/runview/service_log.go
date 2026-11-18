package runview

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ensureLogSource guarantees a run.log tailer is feeding a live RunLogBuffer
// for runID — the log-stream twin of ensureEventSource. For a run this process
// did NOT launch (an external `iterion run`/`resume`, a dispatcher-spawned
// run, or one re-attached across a restart) GetLogBuffer is nil, so the WS log
// subscribe used to fall back to a one-shot replay of run.log and the studio
// needed a full page refresh to see new lines. This starts a fresh in-memory
// buffer plus an fsnotify tailer (disk run.log → buffer) and returns the buffer
// to subscribe to, along with a refcounted release the caller MUST invoke once
// when its subscription ends (the tailer + buffer are dropped on the last
// release). A nil buffer means no source could be started.
//
// Guard the same way as ensureEventSource: only call for runs that are NOT
// active in-process (those feed their buffer directly via the logger tee) and
// are NOT terminal (a finished run's static run.log wants the one-shot replay,
// not a lingering tailer).
func (s *Service) ensureLogSource(ctx context.Context, runID string) (release func(), buf *RunLogBuffer) {
	s.fileSrcMu.Lock()
	if s.logSrcs == nil {
		s.logSrcs = make(map[string]*fileSrcHandle)
	}
	h := s.logSrcs[runID]
	if h == nil {
		h = &fileSrcHandle{done: make(chan struct{})}
		s.logSrcs[runID] = h
		// The buffer is labelled with the run the tailer reads — the one
		// holding the ID now, read without the subscriber's binding — never
		// with the run a subscriber was admitted on.
		h.buf = s.prepareRunLogNoFile(store.WithoutRunDataBinding(ctx), runID) // in-memory buffer GetLogBuffer will return
		startLogSource(s, runID, h.buf, h.done)                                // fsnotify tail run.log → buffer
	}
	h.refs++
	s.fileSrcMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.fileSrcMu.Lock()
			cur := s.logSrcs[runID]
			var drop *RunLogBuffer
			if cur != nil {
				cur.refs--
				if cur.refs <= 0 {
					close(cur.done)
					delete(s.logSrcs, runID)
					drop = cur.buf
				}
			}
			s.fileSrcMu.Unlock()
			if s.logReleaseGap != nil {
				s.logReleaseGap(runID)
			}
			// The tailer's own buffer: one a new subscription or run
			// registered under the ID once the handle was gone stays.
			s.dropRunLog(runID, drop)
		})
	}, h.buf
}

// GetLogBuffer returns the live log buffer for runID, or nil if the
// run is not held by this process. Valid only while the run is
// active; the buffer is Close'd and removed when the run goroutine
// exits.
func (s *Service) GetLogBuffer(runID string) *RunLogBuffer {
	s.runLogsMu.RLock()
	defer s.runLogsMu.RUnlock()
	return s.runLogs[runID]
}

// GetLogBufferOf is GetLogBuffer for a reader bound to one run: the buffer
// only when it was created for the run of this birth (RunLogBuffer.BornOf),
// nil otherwise — a predecessor's buffer still registered under the ID, or
// one whose run could not be read when it was created.
func (s *Service) GetLogBufferOf(runID, birth string) *RunLogBuffer {
	if buf := s.GetLogBuffer(runID); buf != nil && buf.BornOf(birth) {
		return buf
	}
	return nil
}

// recordLogBirth records on buf the birth of the run the buffer is created
// for: the run ctx is bound to (a prepared launch), else the run runID names
// now, read with the caller's identity. A run that cannot be read leaves the
// birth unknown, and the buffer is then served to no bound reader.
func (s *Service) recordLogBirth(ctx context.Context, buf *RunLogBuffer, runID string) {
	if b, ok := store.RunDataBindingFrom(ctx); ok && b.Header().RunID == runID {
		buf.SetBirth(b.Header().BirthID)
		return
	}
	if s.store == nil {
		return
	}
	if run, err := s.store.LoadRun(ctx, runID); err == nil {
		buf.SetBirth(run.BirthID)
	}
}

// InstallLogBufferForTest registers a live buffer for runID holding data, as
// created for the run of birth — a predecessor's buffer a subscription still
// holds after its run was replaced. The returned func removes it.
func (s *Service) InstallLogBufferForTest(runID, birth string, data []byte) func() {
	buf, _ := NewRunLogBuffer("")
	_, _ = buf.Write(data)
	buf.SetBirth(birth)
	s.runLogsMu.Lock()
	s.runLogs[runID] = buf
	s.runLogsMu.Unlock()
	return func() { s.dropRunLog(runID, buf) }
}

// logPositionForRun is the callback shape the store uses to stamp
// Event.LogOffset: returns the current byte total of the per-run log
// buffer, or 0 when no buffer exists yet (bootstrap events emitted
// before prepareRunLog ran). Cheap: one atomic read under an RLock.
func (s *Service) logPositionForRun(runID string) int64 {
	s.runLogsMu.RLock()
	buf := s.runLogs[runID]
	s.runLogsMu.RUnlock()
	if buf == nil {
		return 0
	}
	return buf.Total()
}

// registerRunEngine publishes the live Engine for runID so
// activeDurationForRun can read its monotonic active elapsed. Shares
// the runLogs lifecycle + mutex.
func (s *Service) registerRunEngine(runID string, eng *runtime.Engine, steer chan *runtime.OverrideMsg) {
	s.runLogsMu.Lock()
	s.runEngines[runID] = eng
	if steer != nil {
		s.runSteer[runID] = steer
	}
	s.runLogsMu.Unlock()
}

// unregisterRunEngine drops the Engine (and its steering channel) for
// runID once its goroutine exits.
func (s *Service) unregisterRunEngine(runID string) {
	s.runLogsMu.Lock()
	delete(s.runEngines, runID)
	// The workspace tracker's stat cache is per-run and the tracker
	// outlives every run in this process; evict it here with its peers.
	if s.workspaceTracker != nil {
		s.workspaceTracker.Forget(runID)
	}
	delete(s.runSteer, runID)
	s.runLogsMu.Unlock()
}

// forgetTrackerCacheIfNotLive evicts a run's workspace stat cache unless
// this process is executing that run.
//
// The tracker outlives every run in a studio process, and its per-run
// cache is only dropped by unregisterRunEngine — i.e. for runs this
// process EXECUTED. But the read-only surfaces (a node's file changes, a
// file diff) resolve labels for ARBITRARY runs, including long-finished
// ones this studio never ran, so each one browsed pinned its full stat
// cache for the life of the process: several MiB per run on a repo with
// vendored dependencies, growing monotonically with no ceiling as the
// operator pages through history.
//
// A live run keeps its warm cache — that one is on the write path, where
// re-reading index.json at every node boundary is exactly what the cache
// exists to avoid.
func (s *Service) forgetTrackerCacheIfNotLive(runID string) {
	if s == nil || s.workspaceTracker == nil || runID == "" {
		return
	}
	s.runLogsMu.RLock()
	_, live := s.runEngines[runID]
	s.runLogsMu.RUnlock()
	if !live {
		s.workspaceTracker.Forget(runID)
	}
}

// steerChannelFor returns the live run's override send channel, or nil
// when this process does not hold the run.
func (s *Service) steerChannelFor(runID string) chan *runtime.OverrideMsg {
	s.runLogsMu.RLock()
	defer s.runLogsMu.RUnlock()
	return s.runSteer[runID]
}

// activeDurationForRun is the store.ActiveDurationFn: returns the run's
// monotonic active duration in ms (engine SharedBudget elapsed), or 0
// when the run isn't held by this process or declares no budget. The
// value is CLOCK_MONOTONIC-derived (suspend-excluded) — never
// recomputed from wall-clock timestamps.
func (s *Service) activeDurationForRun(runID string) int64 {
	s.runLogsMu.RLock()
	eng := s.runEngines[runID]
	s.runLogsMu.RUnlock()
	if eng == nil {
		return 0
	}
	return eng.ActiveElapsed().Milliseconds()
}

// prepareRunLog creates a per-run log buffer (also persisting to
// <store-dir>/runs/<runID>/run.log when the store dir is writable)
// and wraps the service's writer + buffer into a per-run logger.
// Returns the buffer for cleanup and the logger to thread through
// both BuildExecutor and runtime.WithLogger so every iterion log line
// emitted during this run is captured for the WS subscribers.
func (s *Service) prepareRunLog(ctx context.Context, runID string) (*RunLogBuffer, *iterlog.Logger) {
	var filePath string
	if path, ok := s.runLogPath(runID); ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			filePath = path
		}
	}
	buf, fileErr := NewRunLogBuffer(filePath)
	if fileErr != nil {
		s.logger.Warn("runview: open run.log for %s: %v — proceeding without disk persistence", runID, fileErr)
	}
	s.recordLogBirth(ctx, buf, runID)

	// A predecessor buffer still registered for this ID (a run goroutine that
	// did not clean up) returns its bail BEFORE this producer registers: two
	// buffers of one run share the same observation, so registering first
	// would JOIN the predecessor's live epoch and its release would never end
	// it. Its subscribers are left to see EOF below.
	s.runLogsMu.Lock()
	old := s.runLogs[runID]
	s.runLogsMu.Unlock()
	if old != nil && old != buf {
		old.releaseLogLane()
	}

	// The lane is registered before the buffer is installed, with the
	// durability the tee actually achieved: a buffer whose file could not be
	// opened is memory-only, and its lane is refused at adoption.
	dur := logMemoryOnly
	if buf.fileTee() {
		dur = logDurable
	}
	s.registerRunLogOrigin(ctx, runID, buf, dur)

	// A fork, so this run's secret matcher lands on a handle of its own:
	// BuildExecutor sets it here (iterion#2822) and the service logger —
	// shared by every run in this process — keeps its own reading.
	perRunLogger := s.logger.WithWriter(io.MultiWriter(s.logger.Writer(), buf))

	s.runLogsMu.Lock()
	s.runLogs[runID] = buf
	s.runLoggers[runID] = perRunLogger
	s.runLogsMu.Unlock()

	// Defensive: a previous run goroutine for this ID didn't fully clean up
	// (the store lock should make this impossible). Its subscribers see EOF
	// rather than dangle forever; its bail was already returned above.
	if old != nil && old != buf {
		old.Close()
	}

	return buf, perRunLogger
}

// runLoggerFor returns the logger a helper must log this run's text on.
// For a run this process holds it is the per-run fork prepareRunLog
// built, carrying the run's secret matcher — which is what the exported
// finalize helpers need: they log a git command's combined output on the
// logger they are HANDED (runtime.RecoverFinalize,
// runtime.CommitUncommittedAndFinalize), and coverage follows the handle,
// not the helper.
//
// For a run held elsewhere — the reaper's sweep over finished runs, a
// finalize after a restart — there is no matcher in this process to hand:
// the guard belongs to the run's own launch. The answer is then a FORK of
// the service logger, never the service logger itself, so the slot those
// lines cross can never be one a live run installed into.
func (s *Service) runLoggerFor(runID string) *iterlog.Logger {
	if s == nil {
		return nil
	}
	s.runLogsMu.RLock()
	l := s.runLoggers[runID]
	s.runLogsMu.RUnlock()
	if l != nil {
		return l
	}
	return s.logger.WithOwnRedactor()
}

// prepareRunLogNoFile is the detached-mode counterpart to
// prepareRunLog: it installs an in-memory-only buffer for runID
// (no file tee) and does NOT return a logger. The runner subprocess
// owns the on-disk run.log; a second writer here would corrupt it.
// File contents reach this buffer via the file_log_source tailer,
// which reads new bytes off disk and pushes them through Write.
func (s *Service) prepareRunLogNoFile(ctx context.Context, runID string) *RunLogBuffer {
	buf, _ := NewRunLogBuffer("")
	s.recordLogBirth(ctx, buf, runID)
	s.runLogsMu.Lock()
	old := s.runLogs[runID]
	s.runLogs[runID] = buf
	s.runLogsMu.Unlock()
	if old != nil && old != buf {
		// Same teardown as the file-backed replacement, outside the lock:
		// the predecessor's bail is returned and its subscribers see EOF.
		old.releaseLogLane()
		old.Close()
	}
	return buf
}

// dropRunLog tears down buf, the per-run buffer its caller created for
// runID (prepareRunLog, prepareRunLogNoFile): closes its subscribers and its
// persisted file, and removes the map entry — only while it still holds buf.
// A buffer registered under the ID since (a new subscription's tailer, a new
// run) is another owner's and stays. Idempotent; a nil buf is a no-op.
func (s *Service) dropRunLog(runID string, buf *RunLogBuffer) {
	if buf == nil {
		return
	}
	s.runLogsMu.Lock()
	if s.runLogs[runID] == buf {
		delete(s.runLogs, runID)
		delete(s.runLoggers, runID)
	}
	s.runLogsMu.Unlock()
	buf.Close()
	// The producer's bail is returned after the buffer's subscribers saw EOF:
	// the lane publishes the end setLogEnd recorded (logEndTerminal by
	// default) and its topic stays retained for a subscriber that parks
	// across the close.
	buf.releaseLogLane()
	s.clearLogProducerFailure(runID)
}

// runLogOwner holds a producer's log bail until it hands the run to its
// goroutine. Transfer runs SYNCHRONOUSLY immediately before the `go` that
// takes the run over; from then on Abandon is a no-op, so a launcher's late
// defer can never release the bail of a run that is alive.
type runLogOwner struct {
	s     *Service
	runID string
	buf   *RunLogBuffer
	mu    sync.Mutex
	held  bool
}

// ownRunLog arms ownership of a producer's buffer, registering its lane when
// prepareRunLog did not (the detached paths, whose buffer has no file tee of
// its own). The caller MUST Transfer before handing the run to its goroutine,
// and Abandon on every path that ends before that hand-off.
func (s *Service) ownRunLog(ctx context.Context, runID string, buf *RunLogBuffer, dur logDurability) *runLogOwner {
	if buf != nil && !buf.laneRegistered() {
		s.registerRunLogOrigin(ctx, runID, buf, dur)
	}
	return &runLogOwner{s: s, runID: runID, buf: buf, held: buf != nil}
}

// Transfer hands the bail to the run's goroutine. Idempotent.
func (o *runLogOwner) Transfer() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.held = false
	o.mu.Unlock()
}

// Abandon returns the bail of a producer that never started: the run's own
// status classifies the epoch (paused keeps a resume possible, terminal ends
// the stream, anything else is lost), then the buffer is dropped. Idempotent;
// a no-op once Transfer handed the bail on.
func (o *runLogOwner) Abandon() {
	if o == nil {
		return
	}
	o.mu.Lock()
	held := o.held
	o.held = false
	o.mu.Unlock()
	if !held {
		return
	}
	if o.buf != nil {
		o.buf.setLogEnd(o.s.classifyRunLogExit(o.runID))
	}
	o.s.dropRunLog(o.runID, o.buf)
}

// classifyRunLogExit reads the run's stored status and classifies it for the
// log stream. A status that cannot be read is LOST: a vanished producer is a
// typed error, never a silent completion.
func (s *Service) classifyRunLogExit(runID string) logEnd {
	run, err := s.store.LoadRun(store.TeamBlind(context.Background()), runID)
	if err != nil || run == nil {
		return logEndLost
	}
	return classifyLogExit(run.Status)
}
