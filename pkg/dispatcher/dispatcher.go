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
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
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
	// leaser is the tracker's claim-lease capability, resolved ONCE at
	// construction. nil for forge trackers (label-based claims carry no
	// record to fence) — announced in the log at startup, never a silent
	// no-op (the ClaimLeaser contract).
	leaser tracker.ClaimLeaser
	runner Runner

	// snapshot holds the most-recently-published immutable Snapshot. The
	// actor is the sole writer (via fireSnapshot); Snapshot() reads it
	// lock-free so dashboard reads never wait on the actor's in-flight
	// tracker I/O. Mirrors the cfg atomic.Pointer precedent above. See
	// docs/adr/028-dispatcher-actor-io-offload.md.
	snapshot atomic.Pointer[Snapshot]

	workspaces *Workspaces
	logger     *iterlog.Logger
	storeDir   string
	hostMarker string
	// claims journals this process's in-flight tracker claims so a
	// successor daemon can release the ones a crash left behind — the
	// only recovery path for external trackers, whose claim label
	// carries no host/PID marker. Nil when StoreDir is unset.
	claims *claimJournal

	state *state
	cmds  chan cmd

	// beforeFinishWorker is a test seam invoked at the start of the off-actor
	// finish worker with the actor-captured value-copy plan. Nil in production.
	beforeFinishWorker func(finishPlan)

	publishMu sync.Mutex
	publisher func(Snapshot)

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	// workersWG counts active goroutines spawned by the actor: the dispatch
	// workers (runWorker) and the off-actor candidate-discovery goroutines
	// (launchDiscovery, ADR-028 Step 2). Stop() blocks on this AFTER the
	// actor exits, so the EngineRunner the workers still reference isn't
	// released until they've all returned — closing the F-CD-1 window where
	// Runner.Close ran while workers were still inside Runner.Dispatch
	// reading the extracted bundle dir. Discovery goroutines guard their
	// cmds send on c.stop, so they too drain promptly once the actor exits.
	workersWG sync.WaitGroup

	// paused, when true, makes tick() skip new dispatches without
	// touching runs in flight or scheduled retries. Toggled via the
	// Pause/Resume public API or the corresponding REST endpoints.
	paused atomic.Bool

	// spendStore backs the daily spend-cap gate. Built once from
	// StoreDir; nil when the store dir is unset or can't host a ledger.
	// The cap limit itself is read fresh from the hot-reloadable config
	// each tick, so changing limits.max_cost_per_day_usd via reload takes
	// effect without a restart.
	spendStore store.SpendStore

	// runReadFailure latches whether the claim watchdog can currently
	// read runs. Atomic because the reaper runs OFF the actor: its
	// abstention is reported on the edges (every card is conserved while
	// it holds), never once per card per pass.
	runReadFailure atomic.Bool

	// shutdownRevertBudget / shutdownReleaseBudget bound the two board
	// writes of the transition-then-release pair — at shutdown AND in
	// every runFinishWorker (the same structural pair; the names keep the
	// site that motivated them). SEPARATE by design (a slow revert must
	// not eat the release's deadline) and injectable so the tests that
	// prove it do not spend the real ones.
	shutdownRevertBudget  time.Duration
	shutdownReleaseBudget time.Duration

	ws *wsBridge
}

// budget returns the configured duration or the production default.
func (c *Dispatcher) budget(configured, def time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return def
}

// cmdBufferSize sizes the actor's command channel. The hazard it guards:
// a burst of cmdRunFinished from up to MaxConcurrent in-flight workers,
// arriving while the actor is busy inside a single finishRun (which may
// make a blocking tracker HTTP call). With a fixed buffer smaller than
// MaxConcurrent, a high-concurrency config could fill the channel and
// wedge workers on the send. Scale the buffer past MaxConcurrent (×2 +
// headroom for ticks / external commands), with a 64 floor for the
// common low-concurrency case. (The deeper fix — never block the actor
// on tracker I/O — is tracked separately; this removes the realistic
// deadlock window.)
func cmdBufferSize(maxConcurrent int) int {
	const floor = 64
	if maxConcurrent > 0 {
		if sized := 2*maxConcurrent + 16; sized > floor {
			return sized
		}
	}
	return floor
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
		cmds:       make(chan cmd, cmdBufferSize(opts.Config.Agent.MaxConcurrent)),
		publisher:  opts.SnapshotPublisher,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		ws:         newWsBridge(opts.Logger),
		claims:     newClaimJournal(opts.StoreDir, opts.Logger),
	}
	if l, ok := opts.Tracker.(tracker.ClaimLeaser); ok {
		c.leaser = l
	} else {
		opts.Logger.Info("dispatcher: tracker %T has no claim-lease capability — lease heartbeats and the claim watchdog are disabled for this tracker (label-based claims cannot be fenced)", opts.Tracker)
	}
	c.cfg.Store(opts.Config)
	// Seed the published snapshot so Snapshot() never reads a nil pointer
	// before the actor's first fireSnapshot. Safe to build here: cfg and
	// state are both set and no other goroutine exists yet.
	seed := c.buildSnapshot()
	c.snapshot.Store(&seed)
	// Wire the daily spend-cap ledger when a store dir is available. The
	// FilesystemRunStore implements store.SpendStore; the runtime runs
	// launched by this dispatcher write into the same <store>/spend/
	// ledger, so the gate sees their cumulative spend. AsSpendStore
	// returns nil for stores that can't host a ledger (cloud Mongo),
	// which disables the gate cleanly.
	if opts.StoreDir != "" {
		if st, err := store.New(opts.StoreDir); err == nil {
			c.spendStore = store.AsSpendStore(st)
		} else {
			opts.Logger.Warn("dispatcher: daily spend cap disabled — open store: %v", err)
		}
	}
	return c, nil
}

// Start runs the actor loop and the polling ticker. Returns
// immediately; use Stop to shut down.
func (c *Dispatcher) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.sweepStaleLocalClaims()
		c.startClaimReaper()
		errtrack.Go("dispatcher.actorLoop", func() { c.actorLoop(ctx) })
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
	host, _ := osHostname()
	if host == "" {
		host = "dispatcher"
	}
	if sweeper, ok := c.tracker.(interface {
		SweepStaleClaims(func(marker string) bool) ([]string, error)
	}); ok {
		cleared, err := sweeper.SweepStaleClaims(func(marker string) bool {
			return isStaleLocalMarker(marker, host)
		})
		if err != nil {
			c.logger.Warn("dispatcher: stale-claim sweep failed: %v", err)
		} else if len(cleared) > 0 {
			c.logger.Info("dispatcher: released %d stale claim(s) from dead local PIDs: %v", len(cleared), cleared)
		}
	}
	// Journal-based sweep — the only recovery path for external adapters
	// (github/forgejo/gitlab), whose claim label carries no host/PID
	// marker the tracker-side sweep above could key on. Entries whose
	// recorded marker belongs to a dead local PID are released with the
	// idempotent Release (an entry journalled just before a crash may
	// never have reached the tracker — releasing an unclaimed issue is
	// not an error). Entries from live PIDs (another daemon sharing the
	// store dir) stay untouched. For the native tracker this is a
	// redundant second net behind SweepStaleClaims; Release tolerates
	// the already-swept case via ErrNotFound.
	c.sweepJournalledClaims(host)
}

// sweepJournalledClaims releases journal entries left by dead local
// dispatcher PIDs. See sweepStaleLocalClaims for the safety argument.
func (c *Dispatcher) sweepJournalledClaims(host string) {
	entries := c.claims.Load()
	if len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var released, stranded []string
	for _, e := range entries {
		if !isStaleLocalMarker(e.Marker, host) {
			if journalDeclineIsPermanent(e.Marker) {
				stranded = append(stranded, e.Identifier+" ("+e.Marker+")")
			}
			continue
		}
		if err := c.tracker.Release(ctx, e.IssueID, e.Marker); err != nil &&
			!errors.Is(err, tracker.ErrNotFound) &&
			!errors.Is(err, tracker.ErrClaimConflict) {
			// Keep the entry: the next boot retries (e.g. the forge API
			// was briefly unreachable).
			c.logger.Warn("dispatcher: release journalled claim %s: %v (will retry next start)", e.Identifier, err)
			continue
		}
		c.claims.Remove(e.IssueID)
		released = append(released, e.Identifier)
	}
	// PERMANENT abstentions are NAMED, once per boot: a marker no future
	// boot can ever prove dead (unparsable shape, pid <= 1) is — on an
	// external tracker, where this sweep is the ONLY recovery path — a
	// claim label stranded for ever with no trace. Transient declines
	// (live pid = another daemon sharing the store; foreign host = not
	// ours to judge) stay silent: warning on them is a storm in every
	// legitimate multi-daemon setup.
	if len(stranded) > 0 {
		c.logger.Warn("dispatcher: boot sweep kept %d journalled claim(s) whose markers can NEVER be proven dead — if their owners are gone, release them by hand: %v",
			len(stranded), stranded)
	}
	if len(released) > 0 {
		c.logger.Info("dispatcher: released %d journalled claim(s) from dead local PIDs: %v", len(released), released)
	}
}

// parseLocalMarker splits a claim marker shaped "<host>-<pid>" (with an
// optional watchdog "reaper:" prefix — a reaper that dies mid-disposition
// must be sweepable like any other dead owner, or the expand/contract
// rollback would strand every card it held). ok=false for any other
// shape, or a pid <= 1: markers no probe can ever interpret. The ONE
// parser both boot-sweep predicates read — a second copy is how the two
// drift apart.
func parseLocalMarker(marker string) (host string, pid int, ok bool) {
	marker = strings.TrimPrefix(marker, reaperMarkerPrefix)
	dash := strings.LastIndexByte(marker, '-')
	if dash <= 0 || dash == len(marker)-1 {
		return "", 0, false
	}
	pid, err := strconv.Atoi(marker[dash+1:])
	if err != nil || pid <= 1 {
		return "", 0, false
	}
	return marker[:dash], pid, true
}

// isStaleLocalMarker returns true iff marker is shaped "<host>-<pid>",
// host matches the current daemon's host, AND pid is not a live process.
// Returns false for any other shape so we never touch a marker we can't
// confidently interpret.
func isStaleLocalMarker(marker, host string) bool {
	mhost, pid, ok := parseLocalMarker(marker)
	if !ok || mhost != host {
		return false
	}
	// A live PID — or one we can't probe (different user, or any
	// non-Unix platform) — counts as "not stale": never reclaim a claim
	// we can't confidently prove is dead. See localPidGone in
	// proc_unix.go / proc_windows.go.
	return localPidGone(pid)
}

// journalDeclineIsPermanent reports that a journalled marker the boot
// sweep declined can NEVER become releasable by a future boot on this
// host: the shape is unparsable or the pid <= 1 (isStaleLocalMarker
// refuses those unconditionally). A live pid or a foreign host is a
// TRANSIENT decline — the owner may release it, or its host's own boot
// will. (On Windows localPidGone abstains for every pid, so live-pid
// declines there are also permanent in practice — accepted: naming them
// all would storm on the one platform where none is provable.)
func journalDeclineIsPermanent(marker string) bool {
	_, _, ok := parseLocalMarker(marker)
	// Parseable + pid > 1 is always transient: same-host = the pid may
	// die; foreign host = its own boot sweep judges it. An UNPARSABLE
	// marker is permanent regardless of host — deliberately stricter
	// than the pre-unification predicate, which read a foreign host's
	// shapeless marker as transient although no boot anywhere can ever
	// admit it (isStaleLocalMarker refuses the shape on every host).
	return !ok
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

// Snapshot returns a consistent view of the actor's state. The read is
// lock-free: it loads the most-recently-published immutable Snapshot
// (written by the actor via fireSnapshot) and never waits on the actor
// goroutine — so a dashboard read returns promptly even while the actor
// is blocked inside a slow synchronous tracker call. Before Start it
// returns the construction-time seed; after Stop it returns the
// last-published state. See docs/adr/028-dispatcher-actor-io-offload.md.
func (c *Dispatcher) Snapshot() Snapshot {
	// New() always seeds the pointer before returning, so Load() is never
	// nil. A nil-fallback to buildSnapshot() would read c.state off the
	// actor goroutine — the very race this read path removes — so we don't.
	return *c.snapshot.Load()
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
	// The per-card writes drain in PARALLEL: entries are independent
	// (distinct issues, thread-safe tracker), and a sequential drain made
	// the wall N × (revert budget + release budget) — under a default
	// terminationGracePeriodSeconds the SIGKILL arrived before the tail
	// and those claims were never released. The bound is now ONE card's
	// budgets, whatever the fleet was carrying.
	var drain sync.WaitGroup
	// Bounded parallelism, and each card's budgets start when its TURN
	// starts, not at a shared T0: unbounded goroutines against a tracker
	// that serializes (the FS store is one mutex; a forge client rate-
	// limits) burn one shared window while queued — measured at N=20 with
	// production budgets, 7 claims leaked in 10s where the sequential
	// drain freed all 20. With 4 in flight a card waits on at most 3
	// calls ahead of it inside the tracker, which any budget dwarfs.
	sem := make(chan struct{}, 4)
	for _, r := range c.state.running {
		// Shutdown is an internal interruption (the local twin of the cloud
		// runner drain): failed_resumable, auto-resumed on the next start.
		if r.Cancel != nil {
			r.Cancel(runtime.ErrRunInterrupted)
		}
		drain.Add(1)
		go func(r *runningEntry) {
			defer drain.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// SEPARATE budgets. The revert opens with a RefreshStates round
			// trip, so sharing one deadline lets a merely slow tracker spend
			// the whole of it and leave the release unsent — and the release
			// is the half shutdown exists for: an unreleased claim hides the
			// card from the next daemon's candidate listing.
			// The revert keeps the release's default (not a shorter one): a
			// tracker slow enough to blow a tighter revert budget but not the
			// release's would leave the card in_progress WITHOUT a claim — the
			// least recoverable shape (wrong state for ListEligible, no claim
			// for the reaper to list).
			revCtx, revCancel := context.WithTimeout(context.Background(), c.budget(c.shutdownRevertBudget, 5*time.Second))
			// Transition FIRST, release LAST — the order the finish worker and
			// the parked reconciler both keep, and for the same reason: a
			// release opens the card to the next claimant immediately, and the
			// revert's own guard ("is it still in_progress?") cannot tell OUR
			// in_progress from a SUCCESSOR's. Releasing first therefore let a
			// shutting-down daemon drag a card back into the launch column
			// while a fresh run was already working it. Fenced, so a claim
			// that moved on refuses the revert instead of clobbering it.
			c.revertTransition(revCtx, r.IssueID, r.Identifier, r.TransitionedFromState, currentTarget, r.claim)
			revCancel()
			relCtx, relCancel := context.WithTimeout(context.Background(), c.budget(c.shutdownReleaseBudget, 5*time.Second))
			c.releaseClaimSess(relCtx, r.IssueID, r.Identifier, r.claim)
			relCancel()
			c.stopClaimSession(r)
		}(r)
	}
	drain.Wait()
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
	// Publish the immutable copy into the lock-free read path before the
	// fan-out, so Snapshot() readers see this state without touching the
	// actor. The actor is the sole writer of c.snapshot.
	c.snapshot.Store(&snap)
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
		LastTickAt:         c.state.lastTickAt,
		PollingIntervalS:   cfg.PollingInterval().Seconds(),
		StallTimeoutS:      cfg.StallTimeout().Seconds(),
		Paused:             c.paused.Load(),
		LastTrackerError:   c.state.lastTrackerErr,
		LastTrackerErrorAt: c.state.lastTrackerErrAt,
		CostCap:            c.state.costCap,
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
	skids := make([]string, 0, len(c.state.dispatchSkips))
	for id := range c.state.dispatchSkips {
		skids = append(skids, id)
	}
	sort.Strings(skids)
	for _, id := range skids {
		snap.DispatchSkips = append(snap.DispatchSkips, c.state.dispatchSkips[id])
	}
	return snap
}

func copyIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	return maps.Clone(in)
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
