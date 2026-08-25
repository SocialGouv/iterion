package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// turnDebounce coalesces a burst of turn-boundary events (a node firing
// ten tool calls in a row) into a single evaluation once the activity
// quiets — so a busy turn costs one LLM call, not ten.
const turnDebounce = 3 * time.Second

// recentEventsCap bounds how many rendered events the supervisor prompt
// carries. Keeps the evaluation prompt small and prompt-cache-stable.
const recentEventsCap = 40

// Observer streams a supervised run's events. *runview.Service
// satisfies it via ObserveRun; the seam keeps pkg/supervise free of a
// runview import (so the engine can spawn a coordinator without an
// import cycle) and lets the coordinator be tested with a hand-fed
// channel.
type Observer interface {
	ObserveRun(ctx context.Context, runID string) (<-chan *store.Event, func(), error)
}

// Injector enqueues a steering message, optionally scoped to a node.
// *runview.Service satisfies it via Inject (which wraps QueueMessage +
// WithMessageNode), so node-scoping, the terminal-state guard, and the
// studio inbox event come for free.
type Injector interface {
	Inject(ctx context.Context, runID, nodeID, text string) error
}

// Coordinator watches one supervised run and drives one supervisor bot.
// It mirrors pkg/server.watchCoordinator: a single goroutine owns all
// mutable state and consumes the run's event stream; the trigger is an
// LLM decision rather than a kanban transition. Injection reuses
// runview.Service.QueueMessage, so message id, terminal-state guard,
// node-scoping, and the studio inbox event stay in lockstep with
// operator-typed messages.
type Coordinator struct {
	obs    Observer
	inj    Injector
	runID  string
	spec   Spec
	eval   Evaluator
	logger *iterlog.Logger

	ctx    context.Context
	cancel func()
	done   chan struct{}

	// --- owned by the run() goroutine; no locks ---
	startedAt time.Time
	// activeNodes tracks every currently-executing node (parallel
	// branches run concurrently); a single "the active node" would be
	// permanently cleared by an unrelated sibling finishing while the
	// watched node still runs.
	activeNodes map[string]struct{}
	// lastWatchedActive is the most recently started watched node —
	// the injection scope for a node-scoped supervisor.
	lastWatchedActive string
	monitors          []Monitor
	// seedCount marks the prefix of monitors that came pre-seeded from
	// the Spec (DSL `monitors:` / CLI --monitor). Seeded monitors are
	// declarative and broad, so their matches honour the cooldown;
	// bot-registered ones (the rest of the slice) were chosen by the
	// bot for precision and bypass it.
	seedCount int
	recent    []string
	last      *Decision
	// evalCount counts COMPLETED evaluations against MaxEvals;
	// evalFailures counts consecutive transport/model failures, which do
	// not consume the budget but park supervision at maxEvalFailures.
	evalCount    int
	evalFailures int
	inTokens     int
	outTokens    int
	lastEvalAt   time.Time
	finished     bool // bot signalled Done; re-armed by a bot-registered monitor match
}

// New builds a Coordinator from the Observer + Injector seams.
// *runview.Service satisfies both, so production callers pass it twice
// (runs, runs). eval may be nil, in which case a production LLMEvaluator
// is used. Returns nil when prerequisites are missing — supervision is
// an enhancement, never a hard dependency.
func New(obs Observer, inj Injector, runID string, spec Spec, eval Evaluator, logger *iterlog.Logger) *Coordinator {
	if obs == nil || inj == nil || runID == "" {
		return nil
	}
	if eval == nil {
		eval = NewLLMEvaluator()
	}
	return &Coordinator{
		obs:         obs,
		inj:         inj,
		runID:       runID,
		spec:        spec.withDefaults(),
		eval:        eval,
		logger:      logger,
		activeNodes: make(map[string]struct{}),
		monitors:    append([]Monitor(nil), spec.Monitors...),
		seedCount:   len(spec.Monitors),
		done:        make(chan struct{}),
	}
}

// Start begins observing in a background goroutine. ctx bounds the
// coordinator's life (cancel it, or call Close, to stop).
func (c *Coordinator) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.startedAt = time.Now()
	go c.run()
}

// Close stops the coordinator and waits for the worker to drain. A
// coordinator that was never Started has no worker to wait for.
func (c *Coordinator) Close() {
	if c == nil || c.ctx == nil {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	<-c.done
}

// Done returns a channel closed when the coordinator's worker exits
// (run terminated or Close called).
func (c *Coordinator) Done() <-chan struct{} { return c.done }

func (c *Coordinator) run() {
	defer close(c.done)

	events, release, err := c.obs.ObserveRun(c.ctx, c.runID)
	if err != nil {
		c.warn("supervise[%s]: cannot observe run %s: %v", c.spec.Name, c.runID, err)
		return
	}
	defer release()

	// One startup line so an operator (and a dogfood log) can tell a
	// spawned-but-silent supervisor from one that never spawned.
	c.info("supervise[%s]: watching run %s (nodes %v, cooldown %s, max_evals %d)",
		c.spec.Name, c.runID, c.spec.Watches, c.spec.Cooldown, c.spec.MaxEvals)

	var debounce *time.Timer
	var debounceC <-chan time.Time
	var debounceOpenedAt time.Time
	// pending defers a cooldown-suppressed SEEDED monitor match to the
	// cooldown's expiry instead of dropping it: the reason string carries
	// the rendered trigger event, so the signal survives even if the
	// event is evicted from the recent ring meanwhile. One slot — the
	// first suppressed match wins; later ones add nothing (the eval that
	// eventually fires sees the whole recent window anyway).
	var pending *time.Timer
	var pendingC <-chan time.Time
	pendingWake := ""
	// Stop both timers on every exit path (ctx cancel, event-channel
	// close, terminal event) so a run that ends mid-debounce doesn't
	// leave a live timer until it fires.
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
		if pending != nil {
			pending.Stop()
		}
	}()
	armDebounce := func() {
		if debounce == nil {
			debounce = time.NewTimer(turnDebounce)
			debounceOpenedAt = time.Now()
			debounceC = debounce.C
			return
		}
		if debounceC == nil {
			debounceOpenedAt = time.Now()
		} else if time.Since(debounceOpenedAt) >= c.spec.Cooldown {
			// A sustained boundary stream (a busy claude_code node emits
			// one every few seconds) would otherwise push the deadline
			// forward forever and starve the periodic wake entirely. Once
			// the window has been open a full cooldown, let it fire.
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(turnDebounce)
		debounceC = debounce.C
	}
	armPending := func(reason string) {
		if pendingC != nil {
			return
		}
		pendingWake = reason
		wait := c.spec.Cooldown - time.Since(c.lastEvalAt) + 100*time.Millisecond
		if wait < 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		if pending == nil {
			pending = time.NewTimer(wait)
		} else {
			if !pending.Stop() {
				select {
				case <-pending.C:
				default:
				}
			}
			pending.Reset(wait)
		}
		pendingC = pending.C
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			c.ingest(evt)
			if IsTerminal(evt) {
				return
			}
			// Reconstruct state from history without acting on it — a
			// supervisor only steers activity that happened after it
			// attached.
			if evt.Timestamp.Before(c.startedAt) {
				continue
			}
			// The inbox family echoes queued-message TEXT — including the
			// supervisor's own steering — back onto the stream. Matching
			// it would loop the coordinator on itself (one intervention →
			// its own text re-fires the marker → another eval → …) until
			// the eval budget is gone. Ingested above (it stays visible in
			// `recent`), never matched or treated as a boundary.
			if isInboxEvent(evt.Type) {
				continue
			}
			if !c.armed() {
				continue
			}
			// A deferred wake whose timer expired while the watched node
			// was gone (end of a pass) re-arms when the node comes back:
			// the give-up marker was emitted as the pass wrapped up, and
			// the next pass is exactly who should hear about it.
			if pendingWake != "" && pendingC == nil &&
				evt.Type == store.EventNodeStarted && c.spec.watchesNode(evt.NodeID) {
				reason := pendingWake
				pendingWake = ""
				armPending(reason)
			}
			if matched, registered := c.matchesMonitor(evt); matched {
				if registered {
					// Bot-chosen signal: evaluate immediately, bypassing
					// cooldown, and re-arm a done supervisor.
					c.finished = false
					c.evaluate(fmt.Sprintf("monitor matched: %s", RenderEvent(evt)), true)
				} else if !c.finished {
					// Pre-seeded (declarative) monitors are broad; their
					// matches honour the cooldown — but a suppressed match
					// is DEFERRED to the cooldown's expiry, never dropped
					// (a give-up marker fires once; losing it kills the
					// monitor lane's whole purpose).
					reason := fmt.Sprintf("monitor matched: %s", RenderEvent(evt))
					if suppressed := c.evaluate(reason, false); suppressed {
						armPending(reason)
					}
				}
			} else if !c.finished && IsTurnBoundary(evt) {
				armDebounce()
			}
		case <-debounceC:
			debounceC = nil
			if c.armed() && !c.finished {
				c.evaluate("turn_boundary", false)
			}
		case <-pendingC:
			pendingC = nil
			if c.armed() && !c.finished {
				reason := pendingWake
				pendingWake = ""
				if suppressed := c.evaluate(reason+" (deferred by cooldown)", false); suppressed {
					// Another wake consumed the slot first — push again.
					armPending(reason)
				}
			}
			// Disarmed: keep pendingWake — a fresh watched node_started
			// re-arms it (see the events case) instead of dropping the
			// one-shot signal.
		}
	}
}

// isInboxEvent reports whether evt belongs to the user-message inbox
// family, whose Data carries queued-message text verbatim.
func isInboxEvent(t store.EventType) bool {
	switch t {
	case store.EventUserMessageQueued, store.EventUserMessageDelivered,
		store.EventUserMessageConsumed, store.EventUserMessageCancelled:
		return true
	}
	return false
}

// ingest folds an event into the coordinator's view: tracks the set of
// active nodes and keeps a bounded ring of rendered recent events.
func (c *Coordinator) ingest(evt *store.Event) {
	switch evt.Type {
	case store.EventNodeStarted:
		if evt.NodeID != "" {
			c.activeNodes[evt.NodeID] = struct{}{}
		}
		// A freshly-started watched node re-arms a done supervisor.
		if c.spec.watchesNode(evt.NodeID) {
			c.finished = false
			c.lastWatchedActive = evt.NodeID
		}
	case store.EventNodeFinished:
		delete(c.activeNodes, evt.NodeID)
		if evt.NodeID == c.lastWatchedActive {
			c.lastWatchedActive = ""
			// Another watched node may still be running.
			for id := range c.activeNodes {
				if c.spec.watchesNode(id) {
					c.lastWatchedActive = id
					break
				}
			}
		}
	}
	c.recent = append(c.recent, RenderEvent(evt))
	if len(c.recent) > recentEventsCap {
		c.recent = c.recent[len(c.recent)-recentEventsCap:]
	}
}

// armed reports whether any currently-active node is watched. Whole-run
// supervisors (empty Watches) are always armed.
func (c *Coordinator) armed() bool {
	if len(c.spec.Watches) == 0 {
		return true
	}
	for id := range c.activeNodes {
		if c.spec.watchesNode(id) {
			return true
		}
	}
	return false
}

// matchesMonitor reports whether any monitor fires on evt, and whether
// at least one of the firing monitors was bot-registered (index past the
// pre-seeded prefix) — the class whose matches bypass the cooldown.
func (c *Coordinator) matchesMonitor(evt *store.Event) (matched, registered bool) {
	for i, m := range c.monitors {
		if m.matches(evt) {
			matched = true
			if i >= c.seedCount {
				return true, true
			}
		}
	}
	return matched, false
}

// maxEvalFailures is the consecutive-failure cap after which the
// coordinator stops trying: a supervisor with no reachable model would
// otherwise burn its whole eval budget on transport errors in seconds
// and read as spawned-but-silent.
const maxEvalFailures = 3

// evaluate consults the bot and applies its decision. bypassCooldown is
// true for bot-registered monitor wakes (high-signal); seeded-monitor
// and turn-boundary wakes honour the cooldown floor. All honour the
// hard MaxEvals budget (of COMPLETED evaluations — a failed call does
// not consume it, but maxEvalFailures consecutive failures park
// supervision). Returns suppressed=true iff the wake was skipped by the
// cooldown — the one outcome the caller may defer and retry.
func (c *Coordinator) evaluate(reason string, bypassCooldown bool) (suppressed bool) {
	if c.finished {
		return false
	}
	if c.evalFailures >= maxEvalFailures {
		return false
	}
	if c.evalCount >= c.spec.MaxEvals {
		if c.evalCount == c.spec.MaxEvals {
			c.info("supervise[%s]: eval budget exhausted (%d) on run %s — supervision paused", c.spec.Name, c.spec.MaxEvals, c.runID)
			c.evalCount++ // log once
		}
		return false
	}
	if !bypassCooldown && !c.lastEvalAt.IsZero() && time.Since(c.lastEvalAt) < c.spec.Cooldown {
		return true
	}

	in := EvalInput{
		Spec:         c.spec,
		ActiveNode:   c.lastWatchedActive,
		WakeReason:   reason,
		RecentEvents: append([]string(nil), c.recent...),
		Monitors:     append([]Monitor(nil), c.monitors...),
		Last:         c.last,
	}
	dec, usage, err := c.eval.Evaluate(c.ctx, in)
	c.lastEvalAt = time.Now()
	c.inTokens += usage.InputTokens
	c.outTokens += usage.OutputTokens
	if err != nil {
		// An eval in flight when the run tears down is cancelled with it
		// — a normal shutdown, not a supervisor malfunction: no warn
		// after the run's terminal summary, no failure counted.
		if c.ctx != nil && c.ctx.Err() != nil &&
			(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			c.info("supervise[%s]: evaluation cancelled at run end (wake=%s)", c.spec.Name, reason)
			return false
		}
		c.evalFailures++
		c.warn("supervise[%s]: evaluation failed on run %s (wake=%s): %v", c.spec.Name, c.runID, reason, err)
		if c.evalFailures >= maxEvalFailures {
			c.warn("supervise[%s]: supervision paused after %d consecutive evaluation failures", c.spec.Name, c.evalFailures)
		}
		return false
	}
	c.evalFailures = 0
	c.evalCount++
	// A silent verdict is the common (and desired) case — log it anyway
	// so an operator can tell "evaluated and chose silence" from "never
	// woke", and see the eval budget drain.
	c.info("supervise[%s]: eval %d/%d (wake=%s) → %s",
		c.spec.Name, c.evalCount, c.spec.MaxEvals, reason, dec.logSummary())
	c.last = dec
	c.applyDecision(dec)
	return false
}

// applyDecision registers any new monitors and enqueues the steering
// message when the bot chose to intervene.
func (c *Coordinator) applyDecision(dec *Decision) {
	if dec == nil {
		return
	}
	for _, m := range dec.Watch {
		// Deduplicate: the eval prompt shows the current list and asks
		// which patterns to KEEP watching, so a bot that re-emits them
		// verbatim would otherwise grow the slice by its whole set every
		// eval (observed: 4 seed → 24 after 5 evals).
		if !m.isEmpty() && !slices.Contains(c.monitors, m) {
			c.monitors = append(c.monitors, m)
		}
	}
	if dec.Intervene {
		if strings.TrimSpace(dec.Message) != "" {
			c.inject(dec.Message)
		} else {
			// An intervention with no text is a malformed decision; say
			// so instead of silently doing nothing.
			c.warn("supervise[%s]: bot chose intervene with an empty message — dropped", c.spec.Name)
		}
	}
	if dec.Done {
		c.finished = true
	}
}

// inject enqueues a steering message, node-scoped when the supervisor
// watches specific nodes so a late message can't leak into the next
// node. Whole-run supervisors enqueue run-scoped messages.
func (c *Coordinator) inject(text string) {
	scopeNode := ""
	if len(c.spec.Watches) > 0 {
		scopeNode = c.lastWatchedActive
	}
	body := text
	if c.spec.Name != "" {
		body = fmt.Sprintf("[supervisor %s] %s", c.spec.Name, text)
	}
	if err := c.inj.Inject(c.ctx, c.runID, scopeNode, body); err != nil {
		c.warn("supervise[%s]: enqueue to run %s failed: %v", c.spec.Name, c.runID, err)
		return
	}
	c.info("supervise[%s]: 📨 steered run %s (node=%q): %s", c.spec.Name, c.runID, scopeNode, truncate(text, 120))
}

func (c *Coordinator) info(format string, args ...any) {
	if c.logger != nil {
		c.logger.Info(format, args...)
	}
}

func (c *Coordinator) warn(format string, args ...any) {
	if c.logger != nil {
		c.logger.Warn(format, args...)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
