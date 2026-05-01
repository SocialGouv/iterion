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

// runHandle is the in-memory per-run state owned by Manager. It carries
// the cancel func that signals a graceful shutdown to the engine
// goroutine, plus a done channel that's closed when the goroutine
// returns.
type runHandle struct {
	cancel    context.CancelFunc
	done      chan struct{}
	startedAt time.Time
}

// Manager owns the lifecycle of in-process workflow goroutines. A run
// is "active" between Register and Deregister; Cancel signals it to
// stop; Stop drains every active run on server shutdown.
type Manager struct {
	mu      sync.Mutex
	handles map[string]*runHandle
	stopped bool
}

// NewManager creates an empty manager.
func NewManager() *Manager {
	return &Manager{handles: make(map[string]*runHandle)}
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
	}
	return ctx, nil
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
