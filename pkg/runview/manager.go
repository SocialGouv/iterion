package runview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrRunNotActive is returned when a manager operation references a
// run ID that has no in-process handle (either it was never launched
// in this process or it has already terminated).
var ErrRunNotActive = errors.New("runview: run is not active in this process")

// runHandle is the in-memory per-run state owned by Manager. It
// carries the cancel func that signals a graceful shutdown plus a
// done channel that's closed when the run terminates.
//
// Two flavours share this struct:
//
//   - In-process (Pid == 0): cancel is the context.CancelFunc returned
//     by context.WithCancel; done is closed by Deregister when the
//     run goroutine exits.
//
//   - Detached subprocess (Pid > 0): cancel sends SIGTERM to the
//     process group; done is closed by a watcher goroutine that
//     polls process liveness (kill -0) until the runner exits.
type runHandle struct {
	cancel    context.CancelFunc
	done      chan struct{}
	startedAt time.Time
	pid       int // 0 for in-process; non-zero for a detached runner
	// pauseCh is the operator-pause signal channel passed to the engine
	// via WithPauseSignal. RequestPause closes it; the engine observes
	// the close at the next safe boundary, saves a checkpoint, flips
	// the status to paused_operator and returns ErrRunPausedOperator.
	// Nil for detached runners (Phase 1 keeps cross-process pause out
	// of scope — cloud-mode operator pause is a follow-up using NATS).
	pauseCh chan struct{}
	// pauseRequested guards against double-close on pauseCh when
	// multiple RequestPause calls race (e.g. the user double-clicks
	// the Pause button before the run has drained).
	pauseRequested bool
	// leaving is closed by MarkLeaving when the engine call has returned
	// and this handle is on its way out. It is what tells a re-registration
	// apart: a handle with leaving OPEN belongs to a runner that is still
	// working, and a second registration for it must be refused at once;
	// one with leaving CLOSED is worth waiting a moment for. Never nil.
	leaving chan struct{}
	// leavingMarked guards against a double close of leaving.
	leavingMarked bool
}

// Manager owns the lifecycle of in-process workflow goroutines. A run
// is "active" between Register and Deregister; Cancel signals it to
// stop; Stop drains every active run on server shutdown.
type Manager struct {
	mu      sync.Mutex
	handles map[string]*runHandle
	stopped bool
	// handoffGrace bounds the wait for a previous runner to finish leaving
	// (see registerHandoffGrace). Overridable so a test that asserts the
	// REFUSAL path does not have to burn the production grace on every run.
	handoffGrace time.Duration
}

// NewManager creates an empty manager.
func NewManager() *Manager {
	return &Manager{handles: make(map[string]*runHandle), handoffGrace: registerHandoffGrace}
}

// Register installs a new run handle and returns the cancellable ctx
// the engine goroutine should use. Register MUST be called before
// spawning the goroutine — otherwise an immediate Cancel could miss
// the registration and the run would be uncancellable.
//
// The returned ctx inherits from parent so any parent cancellation
// (e.g. server shutdown) propagates as well.
//
// Returns an error if the manager has been Stop'd or a handle is
// already registered for runID (defensive — Service.Launch generates
// IDs that should be unique).
func (m *Manager) Register(parent context.Context, runID string) (context.Context, error) {
	m.awaitPreviousRunner(parent, runID)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return nil, fmt.Errorf("runview: manager is stopped")
	}
	if _, exists := m.handles[runID]; exists {
		return nil, fmt.Errorf("runview: run %q is already registered", runID)
	}
	ctx, cancel := context.WithCancel(parent)
	m.handles[runID] = &runHandle{
		cancel:    cancel,
		done:      make(chan struct{}),
		startedAt: time.Now().UTC(),
		pauseCh:   make(chan struct{}),
		leaving:   make(chan struct{}),
	}
	return ctx, nil
}

// MarkLeaving declares that runID's engine call has returned and the handle
// is now only finishing its teardown. It is the evidence awaitPreviousRunner
// needs to tell "this runner is on its way out, wait for it" from "this
// runner is alive and well, refuse now" — without it, every duplicate
// registration would pay the full handoff grace before failing.
//
// Called by the run goroutine as soon as the engine returns, which is also
// when the STORE already carries the terminal or paused status. The teardown
// that follows is what makes the window wide enough to matter: a completion
// webhook, a supervisor drain, a broker close. Idempotent; a no-op for an
// unknown or already-marked run.
func (m *Manager) MarkLeaving(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[runID]
	if !ok || h.leaving == nil || h.leavingMarked {
		return
	}
	h.leavingMarked = true
	close(h.leaving)
}

// PauseSignal returns the engine-side receive-only pause channel for
// runID, suitable to pass into runtime.WithPauseSignal. The caller is
// expected to wire it into the Engine before the goroutine starts.
// Returns nil + ErrRunNotActive when no handle exists. The channel is
// fresh (not closed) at the time Register returns; RequestPause is the
// only path that closes it.
func (m *Manager) PauseSignal(runID string) (<-chan struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[runID]
	if !ok {
		return nil, ErrRunNotActive
	}
	if h.pauseCh == nil {
		return nil, ErrRunNotActive
	}
	return h.pauseCh, nil
}

// RequestPause asks the engine goroutine for runID to interrupt at
// the next safe boundary. Closing the pause channel is the signal
// the engine watches via WithPauseSignal — the run then transitions
// to paused_operator with a preserved checkpoint and can be resumed
// like a cancelled run. Idempotent (double-call is a no-op);
// ErrRunNotActive when no handle exists or the handle is a detached
// runner (which doesn't carry a pause channel in Phase 1).
func (m *Manager) RequestPause(runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[runID]
	if !ok {
		return ErrRunNotActive
	}
	if h.pauseCh == nil {
		return ErrRunNotActive
	}
	if h.pauseRequested {
		return nil
	}
	h.pauseRequested = true
	close(h.pauseCh)
	return nil
}

// RegisterDetached installs a handle for a runner running as a
// detached subprocess (PID > 0). Cancel is the closure that the
// caller wants invoked when Manager.Cancel(runID) is called — typically
// `func() { syscall.Kill(-pid, syscall.SIGTERM) }`. The caller is
// responsible for closing done when the runner exits.
//
// Unlike Register, this method does NOT create a context — detached
// runners own their own context inside the spawned process, so the
// server-side handle has no ctx to propagate.
//
// It also does NOT wait out a previous runner, deliberately. Its callers have
// ALREADY started the subprocess by the time they get here (spawnDetached
// Start()s, then registers), so waiting would leave two `iterion` processes
// aimed at one run for the duration: the loser dies on the store flock while
// the winner's registration succeeds against a dead PID, and the API reports a
// resume that never happened. An immediate refusal keeps that failure loud.
// A detached handoff grace has to be taken BEFORE the spawn to be safe.
func (m *Manager) RegisterDetached(runID string, pid int, cancel context.CancelFunc, done chan struct{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return fmt.Errorf("runview: manager is stopped")
	}
	if _, exists := m.handles[runID]; exists {
		return fmt.Errorf("runview: run %q is already registered", runID)
	}
	m.handles[runID] = &runHandle{
		cancel:    cancel,
		done:      done,
		startedAt: time.Now().UTC(),
		pid:       pid,
	}
	return nil
}

// registerHandoffGrace bounds how long a re-registration waits for the
// PREVIOUS runner of the same run to finish leaving.
//
// The window it closes: a run parking on a human gate writes
// paused_waiting_human to the STORE, returns ErrRunPaused, and only then does
// the goroutine carrying it call Deregister on its way out. Between those, the
// public signal says "resumable" while the handle is still held — and the
// studio and the pipeline-board sidebar offer Resume on exactly that signal.
// A resume landing in the window used to fail with "already registered",
// which reads as a bug to the operator and is one to an automated chain.
//
// A few hundred milliseconds normally; seconds under load. Five is generous
// enough to absorb a loaded host while still surfacing a genuinely stuck
// previous runner as an error rather than hanging the caller.
const registerHandoffGrace = 5 * time.Second

// awaitPreviousRunner waits, bounded, for an existing handle for runID to
// finish — but ONLY when that handle has been marked leaving. A handle whose
// runner is still working returns immediately, so a genuine
// two-runners-for-one-run attempt fails as fast as it always did instead of
// parking the caller (an HTTP handler, typically) for the whole grace.
//
// It does NOT reserve anything: the caller's own registration check remains
// the authority, so a previous runner that never finishes leaving still
// yields "already registered". This only stops that guard from firing on a
// runner that is already on its way out.
//
// The wait happens OUTSIDE the mutex on purpose: Deregister needs the same
// lock to close done, so holding it here would deadlock the very thing we are
// waiting for.
func (m *Manager) awaitPreviousRunner(ctx context.Context, runID string) {
	m.mu.Lock()
	h, exists := m.handles[runID]
	stopped := m.stopped
	m.mu.Unlock()
	// A manager already stopped is going to refuse the registration anyway;
	// waiting first would only delay saying so.
	if !exists || stopped {
		return
	}
	select {
	case <-h.leaving:
	default:
		return // still working — not a handoff, so not ours to wait out
	}
	grace := m.handoffGrace
	if grace <= 0 {
		grace = registerHandoffGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-h.done:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Deregister removes the handle and closes its done channel. Called
// by the goroutine on its way out, regardless of success/failure.
// Idempotent.
func (m *Manager) Deregister(runID string) {
	m.mu.Lock()
	h, ok := m.handles[runID]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.handles, runID)
	m.mu.Unlock()

	// Close outside the lock to avoid blocking other Manager calls.
	close(h.done)
}

// Cancel signals the engine goroutine for runID to stop. The
// goroutine observes ctx.Done() and translates it into a checkpoint
// + RunCancelled event. Returns ErrRunNotActive if no handle exists.
func (m *Manager) Cancel(runID string) error {
	m.mu.Lock()
	h, ok := m.handles[runID]
	m.mu.Unlock()
	if !ok {
		return ErrRunNotActive
	}
	h.cancel()
	return nil
}

// Active reports whether a handle exists for runID.
func (m *Manager) Active(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.handles[runID]
	return ok
}

// ActiveRuns returns the IDs of every run currently held by the
// manager. Order is undefined.
func (m *Manager) ActiveRuns() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.handles))
	for id := range m.handles {
		out = append(out, id)
	}
	return out
}

// HandleSnapshot is one row in the Snapshot view: the run ID plus the
// in-memory primitives Drain needs (cancel + done) and the optional
// PID so callers can distinguish in-process from detached runners.
type HandleSnapshot struct {
	RunID  string
	Cancel context.CancelFunc
	Done   <-chan struct{}
	PID    int // 0 for in-process; >0 for detached subprocess
}

// Snapshot returns a point-in-time copy of every active handle. Drain
// uses this to issue cancel + wait without holding the manager's lock
// across the wait (which would deadlock with concurrent Deregister).
func (m *Manager) Snapshot() []HandleSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]HandleSnapshot, 0, len(m.handles))
	for id, h := range m.handles {
		out = append(out, HandleSnapshot{
			RunID:  id,
			Cancel: h.cancel,
			Done:   h.done,
			PID:    h.pid,
		})
	}
	return out
}

// Wait blocks until the goroutine for runID completes, or until ctx
// is done. Returns ErrRunNotActive immediately if no handle exists.
func (m *Manager) Wait(ctx context.Context, runID string) error {
	m.mu.Lock()
	h, ok := m.handles[runID]
	m.mu.Unlock()
	if !ok {
		return ErrRunNotActive
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop cancels every active run and waits for them to drain. Used
// during server shutdown — we want every goroutine to reach its
// failRunWithCheckpoint path so the on-disk checkpoint is preserved
// for resume. After ctx expires, any still-running goroutine is
// forcibly forgotten (the goroutine itself keeps running but the
// manager drops its handle); callers should accept that this drops
// a small amount of in-flight progress in favour of bounded
// shutdown latency.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	m.stopped = true
	handles := make([]*runHandle, 0, len(m.handles))
	for _, h := range m.handles {
		handles = append(handles, h)
	}
	m.mu.Unlock()

	// Issue cancel on every active run.
	for _, h := range handles {
		h.cancel()
	}
	// Wait for each to drain (or for shutdown ctx to expire).
	for _, h := range handles {
		select {
		case <-h.done:
		case <-ctx.Done():
			return
		}
	}
}
