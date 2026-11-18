package alert

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/deeplink"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	// DefaultStallTimeout is the no-activity window after which a
	// non-terminal run is flagged as stalled.
	DefaultStallTimeout = 5 * time.Minute
	// defaultNotifyTimeout bounds a single sink delivery.
	defaultNotifyTimeout = 15 * time.Second
	// terminalRetention is how long a finished/failed/cancelled run's
	// state is kept before reaping, to dedupe late duplicate events.
	terminalRetention = 30 * time.Minute
)

// runState tracks the per-run liveness + dedup bookkeeping the Manager
// needs to decide when to fire each alert exactly once per episode.
type runState struct {
	id             string
	name           string
	currentNode    string
	lastProgressAt time.Time
	// lastResumeAt identifies the resume event this execution started from:
	// the tailer REPLAYS it on every reconnect, so only a resume newer than
	// the one already applied starts a new execution (round-3 F1).
	lastResumeAt time.Time
	terminal     bool
	terminalAt   time.Time
	// paused is set while the run is intentionally waiting (human form
	// input or an operator pause) and cleared when it resumes or executes
	// a node. Stall detection is suppressed while paused: a run waiting on
	// a human is NOT stalled, and firing "stalled" for it is a false alarm.
	paused        bool
	stallAlerted  bool
	budgetAlerted map[string]bool // axis -> warning already fired
	exceeded      bool
	failed        bool
	// obs is the registration this episode belongs to, and bails the
	// ObservedLanes handed out to its holders — one per holder (a tailer's
	// ensure, a launch, a detached runner, a reattach), closed by THAT
	// holder's release; the episode's delivery lane stays valid while any
	// bail lives, and ends with the last (H5). The dedup state outlives the
	// bails so a late duplicate stays deduped, while a same-observation
	// re-adoption resets execution state. Written only under m.mu.
	obs   store.RunObservation
	bails []*Bail
}

// delivery is an alert's frozen route, captured in the same critical section
// that constructs the alert: the registration truth of the birth, never a
// dispatch-time lookup by run ID. Dispatch consumes it as captured (F1/R3).
type delivery struct {
	a          Alert
	obs        store.RunObservation
	lanes      []ObservedLane
	registered bool
}

// Manager observes the run event stream and drives alert fan-out.
type Manager struct {
	mu     sync.Mutex
	runs   map[string]*runState
	sinks  []Sink
	logger *iterlog.Logger

	stallTimeout    time.Duration
	notifyTimeout   time.Duration
	baseURL         string
	runLookup       func(runID string, obs store.RunObservation, registered bool) (name string, ok bool)
	humanWaitLookup func(ctx context.Context, runID string, obs store.RunObservation, registered bool, now time.Time) bool
	humanWaitCtx    context.Context
	humanWaitCancel context.CancelFunc
	now             func() time.Time
	// storeSink, when set, receives every fired alert with its captured
	// registration so the host can persist a store-event twin
	// (EventRunHealth) — the ephemeral broker sink is deliberately never
	// persisted (feedback-loop guard), so without this a WS reconnect
	// loses the stall episode. Loop safety is structural: Observe ignores
	// EventRunHealth.
	storeSink func(a Alert, obs store.RunObservation, registered bool)
	// legacyAlertPublish, when set, receives every fired alert as the
	// unpersisted EventAlert broker frame — stamped with the captured
	// observation when the run was registered, unstamped (today's shape)
	// otherwise. It is the carrier unbound and wrapper consumers read.
	legacyAlertPublish func(store.Event)

	// stopped refuses new registrations once Stop ran; dispatches admitted
	// before Stop complete against their captured deliveries (F13).
	stopped atomic.Bool

	stopOnce sync.Once
	stop     chan struct{}
}

// Option configures a Manager.
type Option func(*Manager)

// WithStallTimeout overrides the default stall window. A value <= 0
// disables stall detection entirely (budget + failure alerts still
// fire).
func WithStallTimeout(d time.Duration) Option {
	return func(m *Manager) { m.stallTimeout = d }
}

// WithBaseURL sets the origin used to build /runs/<id> deep links.
func WithBaseURL(u string) Option {
	return func(m *Manager) { m.baseURL = strings.TrimRight(u, "/") }
}

// WithLogger wires a logger (nil-safe; the Manager guards on use).
func WithLogger(l *iterlog.Logger) Option {
	return func(m *Manager) { m.logger = l }
}

// WithRunLookup supplies a resolver for a run's friendly name. It receives
// the registration the run's episode holds (or none): a registered run is
// resolved under its captured observation, never by re-reading whatever run
// holds the ID now. Called at most once per run (the result is cached on
// first event).
func WithRunLookup(fn func(runID string, obs store.RunObservation, registered bool) (string, bool)) Option {
	return func(m *Manager) { m.runLookup = fn }
}

// WithHumanWaitLookup supplies persisted proof that a silent run is waiting
// for human input. It receives the run's captured registration so the root
// read validates under the binding while descendants traverse without it.
// Missing or unreadable proof must return false.
func WithHumanWaitLookup(fn func(context.Context, string, store.RunObservation, bool, time.Time) bool) Option {
	return func(m *Manager) { m.humanWaitLookup = fn }
}

// WithSinks appends delivery sinks.
func WithSinks(sinks ...Sink) Option {
	return func(m *Manager) { m.sinks = append(m.sinks, sinks...) }
}

// WithStoreSink installs the persisted-twin callback: it receives every
// fired alert with the registration captured at its birth (registered
// false for a run nobody registered) so the host appends a durable
// EventRunHealth to the run's event stream under that truth. Invoked from
// the dispatch tail, off the manager lock, panic-contained.
func WithStoreSink(fn func(a Alert, obs store.RunObservation, registered bool)) Option {
	return func(m *Manager) { m.storeSink = fn }
}

// WithLegacyAlertPublish installs the unpersisted EventAlert carrier: the
// manager publishes every fired alert to it inline (before Observe
// returns), stamped with the captured observation when the run was
// registered and unstamped otherwise. Dispatch owns the route decision;
// the callback must not re-resolve the run by ID.
func WithLegacyAlertPublish(fn func(store.Event)) Option {
	return func(m *Manager) { m.legacyAlertPublish = fn }
}

// NewManager builds a Manager. Call Start to begin stall polling.
func NewManager(opts ...Option) *Manager {
	waitCtx, waitCancel := context.WithCancel(context.Background())
	m := &Manager{
		runs:         make(map[string]*runState),
		humanWaitCtx: waitCtx, humanWaitCancel: waitCancel,
		stallTimeout:  DefaultStallTimeout,
		notifyTimeout: defaultNotifyTimeout,
		now:           time.Now,
		stop:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// StallTimeout reports the configured stall window.
func (m *Manager) StallTimeout() time.Duration { return m.stallTimeout }

// Register adopts a registration for runID and transfers the lane's
// ownership to the manager: when adopted is false the caller MUST call
// lane.Close() (the manager kept its own live registration — a
// pause/resume no-op — or refused the registration), when it is true the
// manager closes the lane at the episode's release.
//
// The reset tiers (R3/D3): a different observation closes the old lane and
// rebuilds the episode from scratch (a replacement's alerts dedup against
// its own truth, not its predecessor's); the same observation re-adopted
// after a release resets EXECUTION state only (a resume fires again);
// the same observation with a live lane is a pure no-op (a pause keeps
// its episode).
// Register installs a bail for runID's episode and adopts the lane: every
// holder of the episode (a tailer's ensure, a launch, a detached runner, a
// reattach) holds its OWN bail, and the delivery lane stays valid while any
// bail lives (H5) — a resumed producer whose tailer left keeps registry
// delivery. Each bail's own Close releases exactly that bail; the epoch ends
// with the last one. The same observation NEVER resets the execution — a
// re-registration is a reconnect, not a resume (the resume EVENT starts the
// new execution). A different observation rebuilds the episode.
//
// The resolver runs under m.mu and may do bounded store I/O (a 5 s timeout
// at the call site) — head-of-line blocking of concurrent Observe calls is
// assumed, pre-existing at first sight.
func (m *Manager) Register(runID string, obs store.RunObservation, lane ObservedLane) (*Bail, error) {
	if !obs.Observed() || lane == nil {
		return nil, fmt.Errorf("register %s: %w", runID, store.ErrRunDataBindingRequired)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// The refusal is checked UNDER the lock: Stop/ReleaseAll between an
	// unlocked check and the insert would otherwise leave a live bail after
	// shutdown (F4).
	if m.stopped.Load() {
		return nil, fmt.Errorf("register %s: %w", runID, ErrNotRegistered)
	}
	existed := m.runs[runID] != nil
	hadPrior := existed && m.runs[runID].obs.Observed()
	rs := m.runStateLocked(runID, obs, true)
	bail := &Bail{m: m, runID: runID, lane: lane}
	if rs.obs.Observed() && rs.obs == obs {
		// Same episode, another holder: its own bail joins the live ones.
		// The delivery lane is topic-scoped, so it already serves the new
		// holder's episode; the bail is the holder's ownership token.
		//
		// NO reset here (F1/F3): a re-registration is not a resume — a
		// reconnect to a FINISHED run re-subscribes exactly this way, and a
		// reset would replay its deduped alerts and wipe its terminal
		// state. The execution reset belongs to the resume event, which
		// Observe applies.
		rs.bails = append(rs.bails, bail)
		return bail, nil
	}
	if hadPrior {
		// A different observation holds the ID: close every old bail and
		// rebuild the episode around the new registration (a replacement
		// is a new truth, not a continuation).
		old := rs.bails
		rs.bails = nil
		for _, b := range old {
			b.closeLocked()
		}
	}
	rs.obs = obs
	rs.bails = []*Bail{bail}
	// A FIRST registration of a run nobody registered yet resets no
	// EXECUTION state: the run may already have been observed (the runtime
	// observer feeds Observe before any subscription captures the
	// registration), and wiping its dedup would let the tailer's replay
	// re-fire what already fired (round-3 F3). Its EPISODE fields still
	// belong to whoever held the ID: an unregistered predecessor's pause,
	// name and current node must not leak into the successor (round-4 F2).
	if existed && !hadPrior {
		// The predecessor was OBSERVED but never registered (a capture
		// failure: alerts stay off), so its whole execution state belongs
		// to it — an inherited `terminal`/`failed` would silence the
		// successor's alerts and stalls, and its stale `terminalAt` would
		// reap a live registration (round-4 H-10). A run that IS the same
		// execution (paused for a human) gets its pause back from the
		// tailer's replay of `run_paused`, which Observe re-applies — the
		// replay is the execution's own record, not this registration.
		rs.resetExecutionLocked()
		rs.paused = false
		rs.currentNode = ""
		rs.name = ""
		if m.runLookup != nil {
			if name, ok := m.runLookup(runID, obs, true); ok {
				rs.name = name
			}
		}
	}
	if hadPrior {
		rs.resetEpisodeLocked()
		// A replacement's name resolves under its NEW registration — never
		// inherited from the predecessor's episode, even when the resolver
		// refuses (a successor holds the ID now). (A fresh state already
		// resolved through runStateLocked with these same arguments.)
		if m.runLookup != nil {
			if name, ok := m.runLookup(runID, obs, true); ok {
				rs.name = name
			}
		}
	}
	return bail, nil
}

// resetEpisodeLocked clears EVERYTHING one episode owns, for a replacement
// under the same run ID (a new truth, not a continuation). Every field that
// belongs to an episode is reset here and nowhere else, so a field added
// later cannot inherit silently (round-3 F2: `paused` leaked exactly that
// way).
func (rs *runState) resetEpisodeLocked() {
	rs.resetExecutionLocked()
	rs.name = ""
	rs.currentNode = ""
	rs.paused = false
	rs.lastResumeAt = time.Time{}
}

// resetExecutionLocked clears what one EXECUTION must re-fire: terminal
// bookkeeping, dedup flags and the stall window. Name, observation, the
// pause flag and the resume identity are kept — a pause is not an
// execution's end, and only a NEW resume event clears it.
func (rs *runState) resetExecutionLocked() {
	rs.lastProgressAt = time.Time{}
	rs.terminal = false
	rs.terminalAt = time.Time{}
	rs.stallAlerted = false
	rs.budgetAlerted = make(map[string]bool)
	rs.exceeded = false
	rs.failed = false
}

// Observation reports the registration runID's episode holds and whether a
// lane is live for it. Read-only and lock-scoped: safe to call from
// subscribers, never re-entered from the manager's own critical sections.
func (m *Manager) Observation(runID string) (store.RunObservation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observationLocked(runID)
}

func (m *Manager) observationLocked(runID string) (store.RunObservation, bool) {
	rs := m.runs[runID]
	if rs == nil || len(rs.bails) == 0 {
		return store.RunObservation{}, false
	}
	return rs.obs, true
}

// Bail is one holder's ownership token on an episode's registration. Its
// Close releases exactly the bail it was handed for — never "the newest bail
// of this run ID", which a late holder would otherwise consume from a
// successor's episode (H5). Idempotent: a bail closed twice, or already
// closed by a replacement, is a no-op. Its closed state is guarded by the
// manager's mutex alone — no second lock — so a Close racing a replacement or
// a release-all can never wait on a lock the other holds.
type Bail struct {
	m      *Manager
	runID  string
	lane   ObservedLane
	closed bool // guarded by m.mu
}

// Close releases this bail and closes its lane. The episode keeps its dedup
// state; it ends when its last bail closes.
func (b *Bail) Close() {
	if b == nil {
		return
	}
	b.m.closeBail(b)
}

// closeBail removes b from its run's live bails and closes b's lane. Another
// holder's bail is untouched: a release is scoped to the token, not the ID.
func (m *Manager) closeBail(b *Bail) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rs := m.runs[b.runID]; rs != nil {
		for i, live := range rs.bails {
			if live == b {
				rs.bails = append(rs.bails[:i], rs.bails[i+1:]...)
				break
			}
		}
	}
	b.closeLocked()
}

// closeLocked closes b's lane the first time it runs; m.mu is held.
func (b *Bail) closeLocked() {
	if b.closed {
		return
	}
	b.closed = true
	_ = b.lane.Close()
}

// ReleaseRun ends runID's episode for good — every bail still held for it
// closes (a run's normal termination: no holder outlives the run). Idempotent.
func (m *Manager) ReleaseRun(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseAllLocked(runID)
}

// releaseAllLocked closes every bail of runID's episode — the episode is
// over for good (a reap, a service shutdown). Each bail's own Close stays a
// no-op afterwards.
func (m *Manager) releaseAllLocked(runID string) {
	rs := m.runs[runID]
	if rs == nil {
		return
	}
	bails := rs.bails
	rs.bails = nil
	for _, b := range bails {
		b.closeLocked()
	}
}

// ReleaseAll ends every registration the manager holds. The host calls it
// after the live event streams ended (detection stops earlier, at Stop, and
// in-flight dispatches keep publishing against their captured lanes until
// the lanes close here — publishes after this refuse, F13).
func (m *Manager) ReleaseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.runs {
		m.releaseAllLocked(id)
	}
}

// Observe ingests a single run event. It advances the run's liveness
// heartbeat (every event counts as progress) and fires budget / failure
// alerts. Safe for concurrent use; intended to run in the event-source
// goroutine.
func (m *Manager) Observe(evt store.Event) {
	if evt.RunID == "" {
		return
	}
	// Events that are not the RUN working must not count as progress nor
	// re-arm stall detection.
	//
	//   run_health — the persisted twin of our own alerts: replayed
	//     through the file tail it would reset the once-per-episode dedup
	//     and turn a single stall into an alert every poll window.
	//     Structural loop-freedom lives here.
	//   run_workspace_checkpoint — the runner's own timer, laid to
	//     preserve a pod's work. A run stuck on a frozen key or a hung
	//     tool still ticks, and a safety net that keeps its run reading
	//     as alive would blind the alarm it was laid beside — including
	//     when the tick is a FAILING push, which repeats forever.
	if evt.Type == store.EventRunHealth ||
		evt.Type == store.EventRunWorkspaceCheckpoint {
		return
	}

	m.mu.Lock()
	// Events carry no observation (detection is ID-keyed): a run first
	// seen here resolves its name unregistered, until a registration
	// adopts it. The route of any alert fired in this section is captured
	// below, before the lock releases.
	rs := m.runStateLocked(evt.RunID, store.RunObservation{}, false)

	ts := evt.Timestamp
	if ts.IsZero() {
		ts = m.now()
	}
	if ts.After(rs.lastProgressAt) {
		rs.lastProgressAt = ts
	}
	// The tailer REPLAYS events.jsonl from offset 0 on every (re)start, so
	// an event older than the execution's own resume is a REPLAY of the
	// previous execution: it must not fire an alert, nor mark the live
	// execution terminal (round-4 F1 — the resume identity guard alone only
	// protected the resume event itself). The heartbeat above already ran;
	// the pause flag below still clears on a replayed resume.
	if !rs.lastResumeAt.IsZero() && ts.Before(rs.lastResumeAt) &&
		evt.Type != store.EventRunResumed &&
		evt.Type != store.EventRunAutoResumed &&
		evt.Type != store.EventRunPaused {
		m.mu.Unlock()
		return
	}
	// Any event means the run is alive again — re-arm stall detection so
	// a fresh stall episode can re-fire, and close the episode with a
	// recovered alert so the timeline shows both edges.
	wasStalled := rs.stallAlerted
	rs.stallAlerted = false

	var fired []delivery
	if wasStalled {
		fired = append(fired, m.deliverLocked(m.alertLocked(KindStallRecovered, rs, evt.NodeID,
			"activity resumed", "", 0, ts), rs))
	}
	switch evt.Type {
	case store.EventNodeStarted:
		// A node is executing ⇒ the run is active again, not paused.
		rs.paused = false
		if evt.NodeID != "" {
			rs.currentNode = evt.NodeID
		}
	case store.EventRunPaused, store.EventHumanInputRequested:
		// Intentional wait (human form / operator pause), not a stall —
		// suppress stall detection until the run resumes (see checkStalls).
		// An async question (ADR-081) is NOT a wait: the run keeps
		// executing, and muting stall detection on it would blind the
		// alerting for exactly the long runs that need it.
		if store.IsAsyncHumanInput(evt) {
			break
		}
		rs.paused = true
	case store.EventRunResumed, store.EventRunAutoResumed:
		// A resume IS the episode's new execution: clear what the previous
		// one left so a second failure fires, stalls re-arm and a budget
		// axis warns again (F12). Register never does this — a reconnect is
		// not a resume (F1), and a pause keeps its flag until here (F3).
		// The stall window re-arms FROM the resume moment (the heartbeat
		// above), never from zero — and only a resume NEWER than the one
		// already applied starts a new execution: the tailer REPLAYS the
		// event on every reconnect, and re-applying it would wipe the dedup
		// so the replay re-fires what already fired (round-3 F1). A replay
		// still clears the pause flag — it says "not paused" either way.
		if ts.After(rs.lastResumeAt) {
			// The anchor never regresses: the reset zeroes it, so keep the
			// previous one and take the LATER of the two (round-4 F4).
			prev := rs.lastProgressAt
			rs.resetExecutionLocked()
			if ts.After(prev) {
				rs.lastProgressAt = ts
			} else {
				rs.lastProgressAt = prev
			}
			rs.lastResumeAt = ts
		}
		rs.paused = false
	case store.EventBudgetWarning:
		axis := strData(evt.Data, "dimension")
		if !rs.budgetAlerted[axis] {
			rs.budgetAlerted[axis] = true
			fired = append(fired, m.deliverLocked(m.budgetAlertLocked(KindBudgetWarning, rs, evt, axis, ts), rs))
		}
	case store.EventBudgetExceeded:
		axis := strData(evt.Data, "dimension")
		if !rs.exceeded {
			rs.exceeded = true
			fired = append(fired, m.deliverLocked(m.budgetAlertLocked(KindBudgetExceeded, rs, evt, axis, ts), rs))
		}
	case store.EventRunFailed:
		if !rs.failed {
			rs.failed = true
			rs.terminal = true
			rs.terminalAt = ts
			reason := strData(evt.Data, "error")
			if reason == "" {
				reason = "see run logs"
			}
			a := m.alertLocked(KindRunFailed, rs, evt.NodeID, reason, "", 0, ts)
			// The run_failed event has carried the typed code all
			// along — the in-process path must not drop what the cloud
			// path (OpsDispatcher.classify) surfaces.
			a.FailureCode = strData(evt.Data, "code")
			fired = append(fired, m.deliverLocked(a, rs))
		}
	case store.EventRunFinished, store.EventRunCancelled:
		rs.terminal = true
		rs.terminalAt = ts
	}
	// Most events fire nothing; only snapshot the sink slice (an
	// allocation) when there's actually something to dispatch.
	var sinks []Sink
	if len(fired) > 0 {
		sinks = m.snapshotSinksLocked()
	}
	m.mu.Unlock()

	m.dispatch(sinks, fired)
}

// deliverLocked freezes an alert's route out of the state it was born
// from. Caller holds m.mu.
func (m *Manager) deliverLocked(a Alert, rs *runState) delivery {
	lanes := make([]ObservedLane, 0, len(rs.bails))
	for _, b := range rs.bails {
		lanes = append(lanes, b.lane)
	}
	return delivery{a: a, obs: rs.obs, lanes: lanes, registered: len(lanes) > 0}
}

// Start launches the background poll goroutine that fires stall alerts
// and reaps long-terminal run state. It returns immediately; the loop
// stops when ctx is cancelled or Stop is called.
func (m *Manager) Start(ctx context.Context) {
	interval := m.stallTimeout / 2
	if m.stallTimeout <= 0 {
		// Stall disabled — still poll (slowly) to reap terminal state.
		interval = time.Minute
	}
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stop:
				return
			case <-t.C:
				m.checkStalls(m.now())
			}
		}
	}()
}

// Stop terminates the stall-polling goroutine and turns registrations
// away (F13): detection is off, but alerts already admitted keep their
// captured routes — the host releases the lanes at ReleaseAll, after the
// live event streams ended.
func (m *Manager) Stop() {
	m.stopped.Store(true)
	m.stopOnce.Do(func() { m.humanWaitCancel(); close(m.stop) })
}

// checkStalls fires a stall alert for every non-terminal run whose last
// progress is older than the stall timeout (once per episode), reaps
// long-terminal runs, and returns the alerts fired (for testing).
func (m *Manager) checkStalls(now time.Time) []delivery {
	type candidate struct {
		id         string
		state      *runState
		progress   time.Time
		obs        store.RunObservation
		registered bool
	}
	m.mu.Lock()
	var candidates []candidate
	for id, rs := range m.runs {
		if rs.terminal {
			if !rs.terminalAt.IsZero() && now.Sub(rs.terminalAt) > terminalRetention {
				// The episode's lanes close with the reap (the manager's
				// own release pairing — releaseAllLocked, never the public
				// API, which would deadlock under m.mu).
				m.releaseAllLocked(id)
				delete(m.runs, id)
			}
			continue
		}
		if m.stallTimeout <= 0 || rs.paused || rs.lastProgressAt.IsZero() || rs.stallAlerted || now.Sub(rs.lastProgressAt) <= m.stallTimeout {
			continue
		}
		// The route is copied at collection, under m.mu: the lookup runs
		// off-lock, and a concurrent Register/Release must not race its
		// read of the state's fields (H1).
		candidates = append(candidates, candidate{id: id, state: rs, progress: rs.lastProgressAt,
			obs: rs.obs, registered: len(rs.bails) > 0})
	}
	m.mu.Unlock()

	var fired []delivery
	for _, c := range candidates {
		waiting := false
		if m.humanWaitLookup != nil {
			// Store I/O must not hold the event observer's mutex. Stop cancels a
			// blocked lookup; failure/no proof leaves stall detection armed.
			ctx, cancel := context.WithTimeout(m.humanWaitCtx, 5*time.Second)
			waiting = m.humanWaitLookup(ctx, c.id, c.obs, c.registered, now)
			cancel()
		}
		m.mu.Lock()
		rs := m.runs[c.id]
		// An event or another polling pass may have changed the candidate
		// while the lookup was in flight. Never notify from that stale view.
		if m.humanWaitCtx.Err() != nil || rs != c.state || rs.terminal || rs.paused || rs.stallAlerted || !rs.lastProgressAt.Equal(c.progress) || waiting {
			m.mu.Unlock()
			continue
		}
		rs.stallAlerted = true
		reason := fmt.Sprintf("no activity for %s", now.Sub(rs.lastProgressAt).Round(time.Second))
		fired = append(fired, m.deliverLocked(m.alertLocked(KindStall, rs, rs.currentNode, reason, "", 0, now), rs))
		m.mu.Unlock()
	}
	m.mu.Lock()
	var sinks []Sink
	if len(fired) > 0 {
		sinks = m.snapshotSinksLocked()
	}
	m.mu.Unlock()
	m.dispatch(sinks, fired)
	return fired
}

// runStateLocked returns (creating if needed) the per-run state and
// resolves its friendly name on first sight, handing the resolver the
// registration the caller already holds — the resolver never re-reads
// whatever run holds the ID now. Caller holds m.mu.
func (m *Manager) runStateLocked(runID string, obs store.RunObservation, registered bool) *runState {
	rs := m.runs[runID]
	if rs == nil {
		rs = &runState{id: runID, budgetAlerted: make(map[string]bool)}
		if m.runLookup != nil {
			if name, ok := m.runLookup(runID, obs, registered); ok {
				rs.name = name
			}
		}
		m.runs[runID] = rs
	}
	return rs
}

func (m *Manager) budgetAlertLocked(kind Kind, rs *runState, evt store.Event, axis string, ts time.Time) Alert {
	used := floatData(evt.Data, "used")
	limit := floatData(evt.Data, "limit")
	pct := 0.0
	if limit > 0 {
		pct = used / limit * 100
	}
	var reason string
	// A warning that explains itself wins over the ratio phrasing: some
	// dimensions carry no axis (no used/limit), and rendering them as
	// "crossed the advisory threshold (0/0)" would announce a ceiling that
	// nothing approached. This is the surface that reaches an operator who
	// is not watching the console, so it must not speak nonsense.
	if detail := strData(evt.Data, "detail"); detail != "" && kind != KindBudgetExceeded {
		return m.alertLocked(kind, rs, evt.NodeID, detail, axis, pct, ts)
	}
	switch {
	case kind == KindBudgetExceeded:
		reason = fmt.Sprintf("%s budget exhausted (%.0f/%.0f)", axis, used, limit)
	case boolData(evt.Data, "advisory"):
		// warn_tokens-style advisory: consumption is unusual, nothing was
		// blocked — suggest a look rather than announce a ceiling.
		reason = fmt.Sprintf("%s crossed the advisory threshold (%.0f/%.0f) — worth auditing what consumed so much", axis, used, limit)
	default:
		reason = fmt.Sprintf("%s budget at %.0f%% (%.0f/%.0f)", axis, pct, used, limit)
	}
	return m.alertLocked(kind, rs, evt.NodeID, reason, axis, pct, ts)
}

func (m *Manager) alertLocked(kind Kind, rs *runState, nodeID, reason, axis string, pct float64, ts time.Time) Alert {
	node := nodeID
	if node == "" {
		node = rs.currentNode
	}
	return Alert{
		Kind:      kind,
		RunID:     rs.id,
		RunName:   rs.name,
		NodeID:    node,
		Reason:    reason,
		Axis:      axis,
		BudgetPct: pct,
		Link:      m.linkLocked(rs),
		Timestamp: ts,
	}
}

func (m *Manager) linkLocked(rs *runState) string {
	if m.baseURL == "" || rs.id == "" {
		return ""
	}
	return deeplink.Run(m.baseURL, rs.id)
}

func (m *Manager) snapshotSinksLocked() []Sink {
	if len(m.sinks) == 0 {
		return nil
	}
	out := make([]Sink, len(m.sinks))
	copy(out, m.sinks)
	return out
}

// dispatch consumes the deliveries captured at birth: the persisted twin
// first (sync, panic-contained as today), then the inline browser leg —
// the registry lane for a registered run, the legacy carrier otherwise —
// and finally the user sinks, detached as always. The route is never
// re-resolved by ID: a successor registered under the same ID in the
// meantime changes nothing for an in-flight delivery (F1/R3).
func (m *Manager) dispatch(sinks []Sink, deliveries []delivery) {
	if len(deliveries) == 0 {
		return
	}
	if m.storeSink != nil {
		for _, d := range deliveries {
			d := d
			func() {
				defer func() {
					if r := recover(); r != nil && m.logger != nil {
						m.logger.Error("alert: store sink panicked for %s run=%s: %v", d.a.Kind, d.a.RunID, r)
					}
				}()
				m.storeSink(d.a, d.obs, d.registered)
			}()
		}
	}
	if m.legacyAlertPublish != nil || deliveriesRegistered(deliveries) {
		for _, d := range deliveries {
			d := d
			m.publishBirth(d)
		}
	}
	if len(sinks) == 0 {
		return
	}
	for _, d := range deliveries {
		if m.logger != nil {
			m.logger.Info("alert: %s run=%s node=%s", d.a.Kind, d.a.RunID, d.a.NodeID)
		}
		for _, s := range sinks {
			s, d := s, d
			go func() {
				// Sinks are user-supplied integrations (webhook, slack,
				// …) — the most likely panic surface in the product. A
				// panic in a detached goroutine takes down the whole
				// process, so contain it here (matching the recover
				// guards on the runtime/dispatcher goroutines).
				defer func() {
					if r := recover(); r != nil && m.logger != nil {
						m.logger.Error("alert: sink panicked for %s run=%s: %v", d.a.Kind, d.a.RunID, r)
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), m.notifyTimeout)
				defer cancel()
				s.Notify(ctx, d.a)
			}()
		}
	}
}

func deliveriesRegistered(deliveries []delivery) bool {
	for _, d := range deliveries {
		if d.registered {
			return true
		}
	}
	return false
}

// publishBirth publishes one alert's browser leg inline along its captured
// route (Q6a): the frame is buffered before Observe returns, shrinking the
// terminal-alert window (NOT closing it — F7). A refusal on the captured
// lane drops the frame with a warning; it is never re-routed by ID (R3).
func (m *Manager) publishBirth(d delivery) {
	evt := store.Event{
		Type:      store.EventAlert,
		RunID:     d.a.RunID,
		NodeID:    d.a.NodeID,
		Timestamp: d.a.Timestamp,
		Data:      d.a.AsEventData(),
	}
	if !d.registered {
		if m.legacyAlertPublish != nil {
			m.legacyAlertPublish(evt)
		}
		return
	}
	stamped, err := store.WithEventObservation(evt, d.obs)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("alert: stamp %s for run=%s: %v — dropped", d.a.Kind, d.a.RunID, err)
		}
		return
	}
	// The delivery lane is topic-scoped: it serves the publish while any
	// bail of the episode lives, so a bail closed mid-dispatch (a holder
	// releasing concurrently) never takes the frame down (H5). A refusal
	// from every bail means the epoch is over — dropped, never re-routed.
	published := false
	var lastErr error
	for _, lane := range d.lanes {
		if err := lane.Publish(stamped); err != nil {
			lastErr = err
			continue
		}
		published = true
		break
	}
	if !published && lastErr != nil && m.logger != nil {
		m.logger.Warn("alert: %s for run=%s refused by its registration lane: %v — dropped", d.a.Kind, d.a.RunID, lastErr)
	}
	if m.legacyAlertPublish != nil {
		m.legacyAlertPublish(stamped)
	}
}

func boolData(d map[string]any, key string) bool {
	if d == nil {
		return false
	}
	v, _ := d[key].(bool)
	return v
}

func strData(d map[string]any, key string) string {
	if d == nil {
		return ""
	}
	if v, ok := d[key].(string); ok {
		return v
	}
	return ""
}

func floatData(d map[string]any, key string) float64 {
	if d == nil {
		return 0
	}
	switch v := d[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}
