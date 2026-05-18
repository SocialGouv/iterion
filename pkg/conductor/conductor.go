// Package conductor implements iterion's long-running dispatcher.
// It polls a tracker.Tracker for eligible issues and dispatches a
// workflow run per issue, with retry, stall detection, hooks, and
// per-state concurrency limits.
//
// Concurrency model: a single goroutine (the actor) owns all mutable
// state. External callers — fsnotify watcher, HTTP handlers, retry
// timers, dispatch goroutines — interact through typed commands sent
// on Conductor.cmds. This mirrors Symphony's GenServer design with
// fewer moving parts.
package conductor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/conductor/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Options is the construction-time wiring for a Conductor.
type Options struct {
	Config     *Config
	Tracker    tracker.Tracker
	Runner     Runner
	Workspaces *Workspaces
	Logger     *iterlog.Logger
	StoreDir   string

	// HostMarker is the claim marker the conductor writes to the
	// tracker when it claims an issue. Defaults to "<hostname>-<pid>".
	HostMarker string

	// SnapshotPublisher, when non-nil, is invoked with each fresh
	// snapshot after a tick. Used to fan snapshots out over WebSocket.
	SnapshotPublisher func(Snapshot)
}

// Conductor is the long-running dispatcher.
type Conductor struct {
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

// New constructs a Conductor with the given Options. It does not start
// the actor goroutine; call Start.
func New(opts Options) (*Conductor, error) {
	if opts.Config == nil {
		return nil, errors.New("conductor: config required")
	}
	if opts.Tracker == nil {
		return nil, errors.New("conductor: tracker required")
	}
	if opts.Runner == nil {
		return nil, errors.New("conductor: runner required")
	}
	if opts.Workspaces == nil {
		return nil, errors.New("conductor: workspaces required")
	}
	if opts.Logger == nil {
		return nil, errors.New("conductor: logger required")
	}
	if opts.HostMarker == "" {
		opts.HostMarker = defaultHostMarker()
	}
	c := &Conductor{
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
		ws:         newWsBridge(),
	}
	c.cfg.Store(opts.Config)
	return c, nil
}

// Start runs the actor loop and the polling ticker. Returns
// immediately; use Stop to shut down.
func (c *Conductor) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		go c.actorLoop(ctx)
	})
}

// Stop signals the actor to exit and waits for it. Safe to call more
// than once. After the actor exits, also blocks until every worker
// goroutine spawned by runWorker returns — Manager.Stop closes the
// EngineRunner immediately after, and that runner's bundleClean()
// would otherwise race the workers still inside Runner.Dispatch
// (see F-CD-1).
func (c *Conductor) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
	})
	<-c.done
	c.workersWG.Wait()
}

// Refresh enqueues an immediate poll tick, bypassing the regular cadence.
func (c *Conductor) Refresh() {
	select {
	case c.cmds <- cmdRefresh{}:
	default:
		// channel full → a tick is already pending, nothing to do.
	}
}

// Cancel asks the conductor to cancel an in-flight dispatch for the
// given issue. The corresponding worker goroutine receives ctx.Done()
// and the issue is released for re-dispatch on the next tick (subject
// to tracker state). No-op after Stop.
func (c *Conductor) Cancel(issueID string) {
	select {
	case c.cmds <- cmdCancel{issueID: issueID}:
	case <-c.stop:
	}
}

// Reload swaps in a fresh config. Typically wired to ConfigWatcher.
// No-op after Stop.
func (c *Conductor) Reload(cfg *Config) {
	select {
	case c.cmds <- cmdReload{cfg: cfg}:
	case <-c.stop:
	}
}

// Pause stops new dispatches without touching runs already in flight
// or pending retries. Idempotent. The change is observed atomically
// by the next tick().
func (c *Conductor) Pause() {
	c.paused.Store(true)
	c.logger.Info("conductor: paused (new dispatches suspended)")
	c.Refresh()
}

// Resume undoes Pause. Idempotent.
func (c *Conductor) Resume() {
	c.paused.Store(false)
	c.logger.Info("conductor: resumed")
	c.Refresh()
}

// IsPaused reports whether new dispatches are currently suspended.
func (c *Conductor) IsPaused() bool { return c.paused.Load() }

// Snapshot returns a consistent view of the actor's state. After Stop
// (or before Start) it returns a zero Snapshot rather than blocking
// the caller indefinitely.
//
// The 5-second cap on the reply select guards against the actor
// panicking between consuming the command and writing the reply
// (the recovery is in actorLoop's defer, but a recovered panic still
// drops the in-flight command). HTTP handlers calling Snapshot during
// shutdown were observed wedged on the bare <-reply read.
func (c *Conductor) Snapshot() Snapshot {
	reply := make(chan Snapshot, 1)
	select {
	case c.cmds <- cmdSnapshot{reply: reply}:
	case <-c.stop:
		return Snapshot{}
	}
	select {
	case s := <-reply:
		return s
	case <-c.stop:
		return Snapshot{}
	case <-time.After(5 * time.Second):
		return Snapshot{}
	}
}

// SetSnapshotPublisher swaps the fan-out hook (used to wire/unwire WS).
func (c *Conductor) SetSnapshotPublisher(fn func(Snapshot)) {
	c.publishMu.Lock()
	c.publisher = fn
	c.publishMu.Unlock()
}

// Config returns the currently-active config pointer.
func (c *Conductor) Config() *Config { return c.cfg.Load() }

// ---------------------------------------------------------------------------
// actor loop
// ---------------------------------------------------------------------------

func (c *Conductor) actorLoop(ctx context.Context) {
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
				c.logger.Error("conductor: panic in tick: %v", r)
			}
		}()
		c.tick(ctx)
	}
	safeCmdApply := func(command cmd) {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("conductor: panic in command %T: %v", command, r)
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

func (c *Conductor) shutdown() {
	for _, r := range c.state.running {
		if r.Cancel != nil {
			r.Cancel()
		}
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
func (c *Conductor) fireSnapshot() {
	snap := c.buildSnapshot()
	c.ws.broadcast(snap)
	c.publishMu.Lock()
	pub := c.publisher
	c.publishMu.Unlock()
	if pub != nil {
		pub(snap)
	}
}

func (c *Conductor) buildSnapshot() Snapshot {
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
		host = "conductor"
	}
	return fmt.Sprintf("%s-%d", host, osPid())
}
