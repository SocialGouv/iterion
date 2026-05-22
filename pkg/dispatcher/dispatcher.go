// Package dispatcher implements iterion's long-running dispatcher.
// It polls a tracker.Tracker for eligible issues and dispatches a
// workflow run per issue, with retry, stall detection, hooks, and
// per-state concurrency limits.
//
// Concurrency model: a single goroutine (the actor) owns all mutable
// state. External callers — fsnotify watcher, HTTP handlers, retry
// timers, dispatch goroutines — interact through typed commands sent
// on Dispatcher.cmds. This mirrors Symphony's GenServer design with
// fewer moving parts.
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Options is the construction-time wiring for a Dispatcher.
type Options struct {
	Config     *Config
	Tracker    tracker.Tracker
	Runner     Runner
	Workspaces *Workspaces
	Logger     *iterlog.Logger
	StoreDir   string

	// HostMarker is the claim marker the dispatcher writes to the
	// tracker when it claims an issue. Defaults to "<hostname>-<pid>".
	HostMarker string

	// SnapshotPublisher, when non-nil, is invoked with each fresh
	// snapshot after a tick. Used to fan snapshots out over WebSocket.
	SnapshotPublisher func(Snapshot)
}

// Dispatcher is the long-running dispatcher.
type Dispatcher struct {
	cfg     atomic.Pointer[Config]
	tracker tracker.Tracker
	runner  Runner

	workspaces *Workspaces
	logger     *iterlog.Logger
	storeDir   string
	hostMarker string

	state *state
	cmds  chan cmd

	publishMu sync.Mutex
	publisher func(Snapshot)

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	// workersWG counts active worker goroutines spawned by runWorker.
	// Stop() blocks on this AFTER the actor exits, so the EngineRunner
	// the workers still reference isn't released until they've all
	// returned — closing the F-CD-1 window where Runner.Close ran
	// while workers were still inside Runner.Dispatch reading the
	// extracted bundle dir.
	workersWG sync.WaitGroup

	// paused, when true, makes tick() skip new dispatches without
	// touching runs in flight or scheduled retries. Toggled via the
	// Pause/Resume public API or the corresponding REST endpoints.
	paused atomic.Bool

	ws *wsBridge
}

// New constructs a Dispatcher with the given Options. It does not start
// the actor goroutine; call Start.
func New(opts Options) (*Dispatcher, error) {
	if opts.Config == nil {
		return nil, errors.New("dispatcher: config required")
	}
	if opts.Tracker == nil {
		return nil, errors.New("dispatcher: tracker required")
	}
	if opts.Runner == nil {
		return nil, errors.New("dispatcher: runner required")
	}
	if opts.Workspaces == nil {
		return nil, errors.New("dispatcher: workspaces required")
	}
	if opts.Logger == nil {
		return nil, errors.New("dispatcher: logger required")
	}
	if opts.HostMarker == "" {
		opts.HostMarker = defaultHostMarker()
	}
	c := &Dispatcher{
		tracker:    opts.Tracker,
		runner:     opts.Runner,
		workspaces: opts.Workspaces,
		logger:     opts.Logger,
		storeDir:   opts.StoreDir,
		hostMarker: opts.HostMarker,
		state:      newState(),
		cmds:       make(chan cmd, 64),
		publisher:  opts.SnapshotPublisher,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		ws:         newWsBridge(opts.Logger),
	}
	c.cfg.Store(opts.Config)
	return c, nil
}

// Start runs the actor loop and the polling ticker. Returns
// immediately; use Stop to shut down.
func (c *Dispatcher) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.sweepStaleLocalClaims()
		go c.actorLoop(ctx)
	})
}

// sweepStaleLocalClaims releases any claim left over from a previous
// dispatcher PID on this host whose process has since died (a daemon
// restart from watchexec, a crash, an operator Ctrl+C). Without this
// sweep, the new daemon's tick() would skip the stale-claimed issues
// forever (ListCandidates filters out claimed=true), and the operator
// would need to edit issue JSONs by hand — see ticket 7221c7be.
//
// Only marker matching "<thishost>-<pid>" is touched. Claims from
// another host stay untouched (legitimately held by a peer dispatcher).
// Markers from a different shape (older or user-set) also stay so we
// don't reset state we don't understand.
func (c *Dispatcher) sweepStaleLocalClaims() {
	sweeper, ok := c.tracker.(interface {
		SweepStaleClaims(func(marker string) bool) ([]string, error)
	})
	if !ok {
		// External adapters (github/forgejo) carry claims as labels;
		// they handle stale claims with their own GC. The interface
		// type-assert keeps the dispatcher loosely coupled.
		return
	}
	host, _ := osHostname()
	if host == "" {
		host = "dispatcher"
	}
	cleared, err := sweeper.SweepStaleClaims(func(marker string) bool {
		return isStaleLocalMarker(marker, host)
	})
	if err != nil {
		c.logger.Warn("dispatcher: stale-claim sweep failed: %v", err)
		return
	}
	if len(cleared) > 0 {
		c.logger.Info("dispatcher: released %d stale claim(s) from dead local PIDs: %v", len(cleared), cleared)
	}
}

// isStaleLocalMarker returns true iff marker is shaped "<host>-<pid>",
// host matches the current daemon's host, AND pid is not a live process.
// Returns false for any other shape so we never touch a marker we can't
// confidently interpret.
func isStaleLocalMarker(marker, host string) bool {
	// markers look like "rog-3158843". Allow underscores in hostname.
	dash := strings.LastIndexByte(marker, '-')
	if dash <= 0 || dash == len(marker)-1 {
		return false
	}
	if marker[:dash] != host {
		return false
	}
	pid, err := strconv.Atoi(marker[dash+1:])
	if err != nil || pid <= 1 {
		return false
	}
	// syscall.Kill(pid, 0) returns nil if the process exists and we
	// have permission to signal it, ESRCH if the PID is gone, EPERM
	// if the process exists under a different user. EPERM = alive.
	err = syscall.Kill(pid, 0)
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPERM) {
		return false
	}
	return errors.Is(err, syscall.ESRCH)
}

// Stop signals the actor to exit and waits for it. Safe to call more
// than once. After the actor exits, also blocks until every worker
// goroutine spawned by runWorker returns — Manager.Stop closes the
// EngineRunner immediately after, and that runner's bundleClean()
// would otherwise race the workers still inside Runner.Dispatch
// (see F-CD-1).
func (c *Dispatcher) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
	})
	<-c.done
	c.workersWG.Wait()
}

// Refresh enqueues an immediate poll tick, bypassing the regular cadence.
func (c *Dispatcher) Refresh() {
	select {
	case c.cmds <- cmdRefresh{}:
	default:
		// channel full → a tick is already pending, nothing to do.
	}
}

// Cancel asks the dispatcher to cancel an in-flight dispatch for the
// given issue. The corresponding worker goroutine receives ctx.Done()
// and the issue is released for re-dispatch on the next tick (subject
// to tracker state). No-op after Stop.
func (c *Dispatcher) Cancel(issueID string) {
	select {
	case c.cmds <- cmdCancel{issueID: issueID}:
	case <-c.stop:
	}
}

// CancelByRunID asks the dispatcher to cancel an in-flight run by its
// RunID. The run console's HTTP cancel handler uses this — manual
// studio launches register their cancel funcs with the runview Manager,
// but dispatcher-spawned runs only live in the dispatcher's state, so
// without this hook the cancel button silently no-ops. Returns true
// when a matching running entry was found and signalled. Returns false
// (and is non-blocking) after Stop.
func (c *Dispatcher) CancelByRunID(runID string) bool {
	reply := make(chan bool, 1)
	select {
	case c.cmds <- cmdCancelByRunID{runID: runID, reply: reply}:
	case <-c.stop:
		return false
	}
	select {
	case got := <-reply:
		return got
	case <-c.stop:
		return false
	}
}

// Reload swaps in a fresh config. Typically wired to ConfigWatcher.
// No-op after Stop.
func (c *Dispatcher) Reload(cfg *Config) {
	select {
	case c.cmds <- cmdReload{cfg: cfg}:
	case <-c.stop:
	}
}

// Pause stops new dispatches without touching runs already in flight
// or pending retries. Idempotent. The change is observed atomically
// by the next tick().
func (c *Dispatcher) Pause() {
	c.paused.Store(true)
	c.logger.Info("dispatcher: paused (new dispatches suspended)")
	c.Refresh()
}

// Resume undoes Pause. Idempotent.
func (c *Dispatcher) Resume() {
	c.paused.Store(false)
	c.logger.Info("dispatcher: resumed")
	c.Refresh()
}

// IsPaused reports whether new dispatches are currently suspended.
func (c *Dispatcher) IsPaused() bool { return c.paused.Load() }

// Snapshot returns a consistent view of the actor's state. After Stop
// (or before Start) it returns a zero Snapshot rather than blocking
// the caller indefinitely.
//
// The 5-second cap on the reply select guards against the actor
// panicking between consuming the command and writing the reply
// (the recovery is in actorLoop's defer, but a recovered panic still
// drops the in-flight command). HTTP handlers calling Snapshot during
// shutdown were observed wedged on the bare <-reply read.
func (c *Dispatcher) Snapshot() Snapshot {
	reply := make(chan Snapshot, 1)
	select {
	case c.cmds <- cmdSnapshot{reply: reply}:
	case <-c.stop:
		return Snapshot{}
	}
	// NewTimer + Stop so an early return via reply/stop doesn't leak a
	// timer goroutine waiting out the full 5s — under shutdown bursts
	// that leaked tens of timers per second.
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case s := <-reply:
		return s
	case <-c.stop:
		return Snapshot{}
	case <-t.C:
		return Snapshot{}
	}
}

// SetSnapshotPublisher swaps the fan-out hook (used to wire/unwire WS).
func (c *Dispatcher) SetSnapshotPublisher(fn func(Snapshot)) {
	c.publishMu.Lock()
	c.publisher = fn
	c.publishMu.Unlock()
}

// Config returns the currently-active config pointer.
func (c *Dispatcher) Config() *Config { return c.cfg.Load() }

// ---------------------------------------------------------------------------
// actor loop
// ---------------------------------------------------------------------------

func (c *Dispatcher) actorLoop(ctx context.Context) {
	defer close(c.done)

	cfg := c.cfg.Load()
	ticker := time.NewTicker(cfg.PollingInterval())
	defer ticker.Stop()

	// safeTick / safeCmdApply wrap each per-iteration unit of work in
	// a deferred recover so that one panicking tracker adapter or
	// command handler can't kill the actor goroutine (which would
	// deadlock Stop() callers waiting on c.done).
	safeTick := func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("dispatcher: panic in tick: %v", r)
			}
		}()
		c.tick(ctx)
	}
	safeCmdApply := func(command cmd) {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("dispatcher: panic in command %T: %v", command, r)
			}
		}()
		command.apply(c, ctx)
	}

	// Kick off an immediate first tick so the user sees activity
	// without waiting for the cadence.
	safeTick()

	for {
		select {
		case <-c.stop:
			c.shutdown()
			return
		case <-ctx.Done():
			c.shutdown()
			return
		case <-ticker.C:
			safeTick()
		case cmd, ok := <-c.cmds:
			if !ok {
				c.shutdown()
				return
			}
			safeCmdApply(cmd)
			// Re-tick ticker cadence when polling interval changes via Reload.
			if cur := c.cfg.Load(); cur.PollingInterval() != cfg.PollingInterval() {
				ticker.Reset(cur.PollingInterval())
				cfg = cur
			}
		}
	}
}

func (c *Dispatcher) shutdown() {
	// Cancel + release: workers are about to drain and the actor
	// goroutine is exiting, so cmdRunFinished can no longer be
	// processed. If we left in-flight claims on disk our own PID
	// would still own them post-shutdown, and ListCandidates'
	// "Claimed=false" filter would hide those issues from the next
	// Start until the operator manually clears them. Release each
	// claim eagerly here (best-effort, detached ctx with a short
	// budget — same pattern as finishRun's release path).
	currentTarget := c.cfg.Load().Agent.RunningState
	for _, r := range c.state.running {
		if r.Cancel != nil {
			r.Cancel()
		}
		relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.tracker.Release(relCtx, r.IssueID, c.hostMarker); err != nil &&
			!errors.Is(err, tracker.ErrNotFound) &&
			!errors.Is(err, tracker.ErrClaimConflict) {
			c.logger.Warn("dispatcher: shutdown release %s: %v", r.Identifier, err)
		}
		// Revert the in-progress transition (best-effort) so tickets
		// snap back to their source state for the next daemon start.
		// Without this, an operator-triggered Ctrl+C would leave every
		// in-flight ticket in `in_progress`, hidden from the next
		// daemon's eligible candidate list until manually dragged back.
		c.revertTransition(relCtx, r.IssueID, r.Identifier, r.TransitionedFromState, currentTarget)
		relCancel()
	}
	for _, e := range c.state.retries {
		if e.Timer != nil {
			e.Timer.Stop()
		}
	}
	c.ws.Stop()
}

// fireSnapshot publishes the current snapshot to the WS bridge and to
// the optional user-supplied publisher.
func (c *Dispatcher) fireSnapshot() {
	snap := c.buildSnapshot()
	c.ws.broadcast(snap)
	c.publishMu.Lock()
	pub := c.publisher
	c.publishMu.Unlock()
	if pub != nil {
		pub(snap)
	}
}

func (c *Dispatcher) buildSnapshot() Snapshot {
	cfg := c.cfg.Load()
	snap := Snapshot{
		Name:               cfg.Name,
		Tracker:            c.tracker.Name(),
		GeneratedAt:        time.Now().UTC(),
		PollingIntervalS:   cfg.PollingInterval().Seconds(),
		StallTimeoutS:      cfg.StallTimeout().Seconds(),
		Paused:             c.paused.Load(),
		LastTrackerError:   c.state.lastTrackerErr,
		LastTrackerErrorAt: c.state.lastTrackerErrAt,
		Slots: SlotsView{
			GlobalMax:    cfg.Agent.MaxConcurrent,
			GlobalUsed:   len(c.state.running),
			PerStateMax:  copyIntMap(cfg.Agent.MaxConcurrentByState),
			PerStateUsed: copyIntMap(c.state.slotsByState),
		},
	}
	ids := make([]string, 0, len(c.state.running))
	for id := range c.state.running {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := c.state.running[id]
		snap.Running = append(snap.Running, RunningView{
			IssueID:       r.IssueID,
			Identifier:    r.Identifier,
			RunID:         r.RunID,
			WorkflowState: r.WorkflowState,
			WorkspacePath: r.WorkspacePath,
			StartedAt:     r.StartedAt,
			LastEventAt:   r.LastEventAt,
			LastEventName: r.LastEventName,
			Attempt:       r.Attempt,
		})
	}
	rids := make([]string, 0, len(c.state.retries))
	for id := range c.state.retries {
		rids = append(rids, id)
	}
	sort.Strings(rids)
	for _, id := range rids {
		e := c.state.retries[id]
		snap.Retries = append(snap.Retries, RetryView{
			IssueID:    e.IssueID,
			Identifier: e.Identifier,
			Attempt:    e.Attempt,
			DueAt:      e.DueAt,
			Error:      e.LastError,
		})
	}
	return snap
}

func copyIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// utilities
// ---------------------------------------------------------------------------

func defaultHostMarker() string {
	host, err := osHostname()
	if err != nil || host == "" {
		host = "dispatcher"
	}
	return fmt.Sprintf("%s-%d", host, osPid())
}
