package sessionboard

import (
	"context"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// turnDebounce coalesces a burst of turn-boundary events into one
// evaluation once the activity quiets — so a busy turn costs one LLM call,
// not ten. Mirrors pkg/supervise.
const turnDebounce = 3 * time.Second

// recentEventsCap bounds how many rendered events the prompt carries
// (keeps it small and prompt-cache-stable).
const recentEventsCap = 40

// Defaults for the curation knobs.
const (
	DefaultCooldown = 45 * time.Second
	DefaultMaxEvals = 20
)

// Observer streams a run's events. *runview.Service satisfies it via
// ObserveRun; the seam keeps this package free of a runview import.
type Observer interface {
	ObserveRun(ctx context.Context, runID string) (<-chan *store.Event, func(), error)
}

// Emitter persists an updated board spec. *runview.Service satisfies it by
// saving via the FileStore. The studio picks the change up by refetching
// the spec as the run's event stream advances (board updates are
// infrequent — bounded by the coordinator's cooldown floor).
//
// TODO(follow-on): push a `sessionboard_updated` event so the studio can
// react without polling, and add a per-bot DSL `session_board:` block so
// curation is opt-in per workflow rather than only via the
// ITERION_SESSION_BOARD env gate.
type Emitter interface {
	Publish(ctx context.Context, runID string, spec Spec) error
}

// Config is the resolved per-run curation configuration.
type Config struct {
	BotID    string        // running bot id, for prompt framing
	Model    string        // claw model spec; empty => auto-detect a cheap model
	Cooldown time.Duration // min wall-clock between evaluations; 0 => default
	MaxEvals int           // hard LLM-call budget; 0 => default
	// Initial seeds the board from a previously-persisted spec (resume),
	// so curation continues from where it left off instead of redrawing.
	Initial Spec
}

func (c Config) withDefaults() Config {
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.MaxEvals <= 0 {
		c.MaxEvals = DefaultMaxEvals
	}
	return c
}

// Coordinator watches one run and curates its board. It mirrors
// pkg/supervise.Coordinator: a single goroutine owns all mutable state and
// consumes the run's event stream; the trigger is an LLM decision that
// emits widget diffs rather than a steering message.
type Coordinator struct {
	obs    Observer
	emit   Emitter
	runID  string
	cfg    Config
	eval   Evaluator
	logger *iterlog.Logger

	ctx    context.Context
	cancel func()
	done   chan struct{}

	// --- owned by the run() goroutine; no locks ---
	startedAt  time.Time
	activeNode string
	recent     []string
	spec       Spec
	lastSeq    int64
	evalCount  int
	lastEvalAt time.Time
}

// New builds a Coordinator. eval may be nil (a production LLMEvaluator is
// used, pinned to cfg.Model). Returns nil when prerequisites are missing —
// curation is an enhancement, never a hard dependency.
func New(obs Observer, emit Emitter, runID string, cfg Config, eval Evaluator, logger *iterlog.Logger) *Coordinator {
	if obs == nil || emit == nil || runID == "" {
		return nil
	}
	cfg = cfg.withDefaults()
	if eval == nil {
		eval = NewLLMEvaluator(cfg.Model)
	}
	return &Coordinator{
		obs:    obs,
		emit:   emit,
		runID:  runID,
		cfg:    cfg,
		eval:   eval,
		logger: logger,
		spec:   cfg.Initial,
		done:   make(chan struct{}),
	}
}

// Start begins observing in a background goroutine.
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

func (c *Coordinator) run() {
	defer close(c.done)

	events, release, err := c.obs.ObserveRun(c.ctx, c.runID)
	if err != nil {
		c.warn("sessionboard: cannot observe run %s: %v", c.runID, err)
		return
	}
	defer release()

	var debounce *time.Timer
	var debounceC <-chan time.Time
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
			if supervise.IsTerminal(evt) {
				// One last evaluation so the board reflects the final
				// state, then stop.
				c.evaluate("run_terminated", true)
				return
			}
			// Reconstruct state from history without acting on it.
			if evt.Timestamp.Before(c.startedAt) {
				continue
			}
			if supervise.IsTurnBoundary(evt) {
				armDebounce()
			}
		case <-debounceC:
			debounceC = nil
			c.evaluate("turn_boundary", false)
		}
	}
}

// ingest folds an event into the coordinator's view.
func (c *Coordinator) ingest(evt *store.Event) {
	if evt == nil {
		return
	}
	if evt.Seq > c.lastSeq {
		c.lastSeq = evt.Seq
	}
	switch evt.Type {
	case store.EventNodeStarted:
		c.activeNode = evt.NodeID
	case store.EventNodeFinished:
		if evt.NodeID == c.activeNode {
			c.activeNode = ""
		}
	}
	c.recent = append(c.recent, supervise.RenderEvent(evt))
	if len(c.recent) > recentEventsCap {
		c.recent = c.recent[len(c.recent)-recentEventsCap:]
	}
}

// evaluate consults the bot and applies its decision. bypassCooldown is
// true for the terminal wake; turn-boundary wakes honour the cooldown
// floor. Both honour the hard MaxEvals budget.
func (c *Coordinator) evaluate(reason string, bypassCooldown bool) {
	if c.evalCount >= c.cfg.MaxEvals {
		if c.evalCount == c.cfg.MaxEvals {
			c.info("sessionboard: eval budget exhausted (%d) on run %s — curation paused", c.cfg.MaxEvals, c.runID)
			c.evalCount++ // log once
		}
		return
	}
	if !bypassCooldown && !c.lastEvalAt.IsZero() && time.Since(c.lastEvalAt) < c.cfg.Cooldown {
		return
	}

	in := EvalInput{
		BotID:        c.cfg.BotID,
		ActiveNode:   c.activeNode,
		WakeReason:   reason,
		RecentEvents: append([]string(nil), c.recent...),
		Current:      c.spec,
	}
	dec, _, err := c.eval.Evaluate(c.ctx, in)
	c.lastEvalAt = time.Now()
	c.evalCount++
	if err != nil {
		c.warn("sessionboard: evaluation failed on run %s: %v", c.runID, err)
		return
	}
	if dec == nil {
		return
	}
	next, changed := ApplyDecision(c.spec, *dec)
	if !changed {
		return
	}
	next.UpdatedSeq = c.lastSeq
	if err := c.emit.Publish(c.ctx, c.runID, next); err != nil {
		// Not committed: c.spec keeps mirroring the last persisted spec, so
		// the next evaluation re-derives the diff against the true board and
		// retries the publish instead of believing the lost update landed.
		c.warn("sessionboard: publish spec for run %s failed: %v", c.runID, err)
		return
	}
	c.spec = next
	c.info("sessionboard: updated board for run %s (v%d, %d widgets)", c.runID, c.spec.Version, len(c.spec.Widgets))
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
