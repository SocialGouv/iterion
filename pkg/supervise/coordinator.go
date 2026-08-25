package supervise

import (
	"context"
	"fmt"
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
	startedAt  time.Time
	activeNode string
	monitors   []Monitor
	recent     []string
	last       *Decision
	evalCount  int
	inTokens   int
	outTokens  int
	lastEvalAt time.Time
	finished   bool // bot signalled Done; re-armed by a monitor match
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
		obs:      obs,
		inj:      inj,
		runID:    runID,
		spec:     spec.withDefaults(),
		eval:     eval,
		logger:   logger,
		monitors: append([]Monitor(nil), spec.Monitors...),
		done:     make(chan struct{}),
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

// Close stops the coordinator and waits for the worker to drain.
func (c *Coordinator) Close() {
	if c == nil {
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
	// Stop the debounce timer on every exit path (ctx cancel, event-channel
	// close, terminal event) so a run that ends mid-debounce doesn't leave a
	// live timer until it fires.
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	armDebounce := func() {
		if debounce == nil {
			debounce = time.NewTimer(turnDebounce)
		} else {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(turnDebounce)
		}
		debounceC = debounce.C
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
			if !c.armed() {
				continue
			}
			if c.matchesMonitor(evt) {
				// High-signal: evaluate immediately, bypassing cooldown,
				// and re-arm if the bot had declared itself done.
				c.finished = false
				c.evaluate(fmt.Sprintf("monitor matched: %s", RenderEvent(evt)), true)
			} else if !c.finished && IsTurnBoundary(evt) {
				armDebounce()
			}
		case <-debounceC:
			debounceC = nil
			if c.armed() && !c.finished {
				c.evaluate("turn_boundary", false)
			}
		}
	}
}

// ingest folds an event into the coordinator's view: tracks the active
// node and keeps a bounded ring of rendered recent events.
func (c *Coordinator) ingest(evt *store.Event) {
	switch evt.Type {
	case store.EventNodeStarted:
		c.activeNode = evt.NodeID
		// A freshly-started watched node re-arms a done supervisor.
		if c.spec.watchesNode(evt.NodeID) {
			c.finished = false
		}
	case store.EventNodeFinished:
		if evt.NodeID == c.activeNode {
			c.activeNode = ""
		}
	}
	c.recent = append(c.recent, RenderEvent(evt))
	if len(c.recent) > recentEventsCap {
		c.recent = c.recent[len(c.recent)-recentEventsCap:]
	}
}

// armed reports whether the supervisor is currently watching the active
// node. Whole-run supervisors (empty Watches) are always armed.
func (c *Coordinator) armed() bool {
	if len(c.spec.Watches) == 0 {
		return true
	}
	return c.activeNode != "" && c.spec.watchesNode(c.activeNode)
}

// matchesMonitor reports whether any registered monitor fires on evt.
func (c *Coordinator) matchesMonitor(evt *store.Event) bool {
	for _, m := range c.monitors {
		if m.matches(evt) {
			return true
		}
	}
	return false
}

// evaluate consults the bot and applies its decision. bypassCooldown is
// true for monitor-match wakes (high-signal); turn-boundary wakes honour
// the cooldown floor. Both honour the hard MaxEvals budget.
func (c *Coordinator) evaluate(reason string, bypassCooldown bool) {
	if c.finished {
		return
	}
	if c.evalCount >= c.spec.MaxEvals {
		if c.evalCount == c.spec.MaxEvals {
			c.info("supervise[%s]: eval budget exhausted (%d) on run %s — supervision paused", c.spec.Name, c.spec.MaxEvals, c.runID)
			c.evalCount++ // log once
		}
		return
	}
	if !bypassCooldown && !c.lastEvalAt.IsZero() && time.Since(c.lastEvalAt) < c.spec.Cooldown {
		return
	}

	in := EvalInput{
		Spec:         c.spec,
		ActiveNode:   c.activeNode,
		WakeReason:   reason,
		RecentEvents: append([]string(nil), c.recent...),
		Monitors:     append([]Monitor(nil), c.monitors...),
		Last:         c.last,
	}
	dec, usage, err := c.eval.Evaluate(c.ctx, in)
	c.lastEvalAt = time.Now()
	c.evalCount++
	c.inTokens += usage.InputTokens
	c.outTokens += usage.OutputTokens
	if err != nil {
		c.warn("supervise[%s]: evaluation failed on run %s: %v", c.spec.Name, c.runID, err)
		return
	}
	// A silent verdict is the common (and desired) case — log it anyway
	// so an operator can tell "evaluated and chose silence" from "never
	// woke", and see the eval budget drain.
	c.info("supervise[%s]: eval %d/%d (wake=%s) → %s",
		c.spec.Name, c.evalCount, c.spec.MaxEvals, reason, dec.logSummary())
	c.last = dec
	c.applyDecision(dec)
}

// applyDecision registers any new monitors and enqueues the steering
// message when the bot chose to intervene.
func (c *Coordinator) applyDecision(dec *Decision) {
	if dec == nil {
		return
	}
	for _, m := range dec.Watch {
		if !m.isEmpty() {
			c.monitors = append(c.monitors, m)
		}
	}
	if dec.Intervene && strings.TrimSpace(dec.Message) != "" {
		c.inject(dec.Message)
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
		scopeNode = c.activeNode
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
