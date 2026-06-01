package alert

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
	terminal       bool
	terminalAt     time.Time
	// paused is set while the run is intentionally waiting (human form
	// input or an operator pause) and cleared when it resumes or executes
	// a node. Stall detection is suppressed while paused: a run waiting on
	// a human is NOT stalled, and firing "stalled" for it is a false alarm.
	paused        bool
	stallAlerted  bool
	budgetAlerted map[string]bool // axis -> warning already fired
	exceeded      bool
	failed        bool
}

// Manager observes the run event stream and drives alert fan-out.
type Manager struct {
	mu     sync.Mutex
	runs   map[string]*runState
	sinks  []Sink
	logger *iterlog.Logger

	stallTimeout  time.Duration
	notifyTimeout time.Duration
	baseURL       string
	runLookup     func(runID string) (name string, ok bool)
	now           func() time.Time

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

// WithRunLookup supplies a resolver for a run's friendly name. Called at
// most once per run (the result is cached on first event).
func WithRunLookup(fn func(runID string) (string, bool)) Option {
	return func(m *Manager) { m.runLookup = fn }
}

// WithSinks appends delivery sinks.
func WithSinks(sinks ...Sink) Option {
	return func(m *Manager) { m.sinks = append(m.sinks, sinks...) }
}

// NewManager builds a Manager. Call Start to begin stall polling.
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		runs:          make(map[string]*runState),
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

// Observe ingests a single run event. It advances the run's liveness
// heartbeat (every event counts as progress) and fires budget / failure
// alerts. Safe for concurrent use; intended to run in the event-source
// goroutine.
func (m *Manager) Observe(evt store.Event) {
	if evt.RunID == "" {
		return
	}

	m.mu.Lock()
	rs := m.runStateLocked(evt.RunID)

	ts := evt.Timestamp
	if ts.IsZero() {
		ts = m.now()
	}
	if ts.After(rs.lastProgressAt) {
		rs.lastProgressAt = ts
	}
	// Any event means the run is alive again — re-arm stall detection so
	// a fresh stall episode can re-fire.
	rs.stallAlerted = false

	var fired []Alert
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
		rs.paused = true
	case store.EventRunResumed:
		rs.paused = false
	case store.EventBudgetWarning:
		axis := strData(evt.Data, "dimension")
		if !rs.budgetAlerted[axis] {
			rs.budgetAlerted[axis] = true
			fired = append(fired, m.budgetAlertLocked(KindBudgetWarning, rs, evt, axis, ts))
		}
	case store.EventBudgetExceeded:
		axis := strData(evt.Data, "dimension")
		if !rs.exceeded {
			rs.exceeded = true
			fired = append(fired, m.budgetAlertLocked(KindBudgetExceeded, rs, evt, axis, ts))
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
			fired = append(fired, m.alertLocked(KindRunFailed, rs, evt.NodeID, reason, "", 0, ts))
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

// Stop terminates the stall-polling goroutine.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
}

// checkStalls fires a stall alert for every non-terminal run whose last
// progress is older than the stall timeout (once per episode), reaps
// long-terminal runs, and returns the alerts fired (for testing).
func (m *Manager) checkStalls(now time.Time) []Alert {
	m.mu.Lock()
	var fired []Alert
	for id, rs := range m.runs {
		if rs.terminal {
			if !rs.terminalAt.IsZero() && now.Sub(rs.terminalAt) > terminalRetention {
				delete(m.runs, id)
			}
			continue
		}
		if m.stallTimeout <= 0 {
			continue
		}
		// A run waiting on a human form (or an operator pause) is not
		// stalled — it's intentionally idle. Don't fire a false alarm.
		if rs.paused {
			continue
		}
		if rs.lastProgressAt.IsZero() || rs.stallAlerted {
			continue
		}
		idle := now.Sub(rs.lastProgressAt)
		if idle <= m.stallTimeout {
			continue
		}
		rs.stallAlerted = true
		reason := fmt.Sprintf("no activity for %s", idle.Round(time.Second))
		fired = append(fired, m.alertLocked(KindStall, rs, rs.currentNode, reason, "", 0, now))
	}
	var sinks []Sink
	if len(fired) > 0 {
		sinks = m.snapshotSinksLocked()
	}
	m.mu.Unlock()

	m.dispatch(sinks, fired)
	return fired
}

// runStateLocked returns (creating if needed) the per-run state and
// resolves its friendly name on first sight. Caller holds m.mu.
func (m *Manager) runStateLocked(runID string) *runState {
	rs := m.runs[runID]
	if rs == nil {
		rs = &runState{id: runID, budgetAlerted: make(map[string]bool)}
		if m.runLookup != nil {
			if name, ok := m.runLookup(runID); ok {
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
	if kind == KindBudgetExceeded {
		reason = fmt.Sprintf("%s budget exhausted (%.0f/%.0f)", axis, used, limit)
	} else {
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
	return m.baseURL + "/runs/" + rs.id
}

func (m *Manager) snapshotSinksLocked() []Sink {
	if len(m.sinks) == 0 {
		return nil
	}
	out := make([]Sink, len(m.sinks))
	copy(out, m.sinks)
	return out
}

func (m *Manager) dispatch(sinks []Sink, alerts []Alert) {
	if len(sinks) == 0 || len(alerts) == 0 {
		return
	}
	for _, a := range alerts {
		if m.logger != nil {
			m.logger.Info("alert: %s run=%s node=%s", a.Kind, a.RunID, a.NodeID)
		}
		for _, s := range sinks {
			s, a := s, a
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), m.notifyTimeout)
				defer cancel()
				s.Notify(ctx, a)
			}()
		}
	}
}

func strData(d map[string]interface{}, key string) string {
	if d == nil {
		return ""
	}
	if v, ok := d[key].(string); ok {
		return v
	}
	return ""
}

func floatData(d map[string]interface{}, key string) float64 {
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
