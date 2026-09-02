package trigger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Evaluator is the consumer side of the spine: it receives an Event (from the
// bus), finds the subscriptions that match, and dispatches each to the right
// effect — Launcher for direct mode, BoardEffect for board mode. Its Handle
// method has the eventbus.Handler signature so the wiring layer can register
// it with bus.Subscribe without the trigger package importing eventbus.
type Evaluator struct {
	subs     SubscriptionStore
	launcher Launcher    // direct mode; nil = direct subs skipped (warn)
	board    BoardEffect // board mode; nil = board subs skipped (warn)
	logger   *iterlog.Logger
}

// EvaluatorOption configures an Evaluator.
type EvaluatorOption func(*Evaluator)

// WithLauncher sets the direct-mode launcher.
func WithLauncher(l Launcher) EvaluatorOption { return func(e *Evaluator) { e.launcher = l } }

// WithBoardEffect sets the board-mode promote effect.
func WithBoardEffect(b BoardEffect) EvaluatorOption { return func(e *Evaluator) { e.board = b } }

// WithLogger sets the leveled logger (nil-safe).
func WithLogger(l *iterlog.Logger) EvaluatorOption { return func(e *Evaluator) { e.logger = l } }

// NewEvaluator builds an Evaluator over a subscription store.
func NewEvaluator(subs SubscriptionStore, opts ...EvaluatorOption) *Evaluator {
	e := &Evaluator{subs: subs}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Handle evaluates one event against the subscription store and fires every
// matching subscription. It never returns the effect errors fatally — a
// single subscription failure is logged and the rest still fire (one bad
// trigger must not silence the others). The signature matches
// eventbus.Handler.
func (e *Evaluator) Handle(ctx context.Context, ev Event) error {
	matched, err := matchingSubscriptions(ctx, e.subs, ev)
	if err != nil {
		return err
	}
	for _, sub := range matched {
		if err := e.applyEffect(ctx, sub, ev, effectOpts{}); err != nil && !errors.Is(err, errEffectOneShotSpent) {
			e.warn("trigger: effect for subscription %s failed: %v", sub.ID, err)
		}
	}
	return nil
}

// matchingSubscriptions returns the enabled, matching, non-observational
// subscriptions an event owes an effect to — the ONE matching prelude shared
// by the bus path (Handle) and the outbox materialization
// (MaterializeEffects), so an admission rule added for one path cannot be
// missed by the other.
func matchingSubscriptions(ctx context.Context, subs SubscriptionStore, ev Event) ([]Subscription, error) {
	// An event already launched by an authoritative path (today: the inline
	// forge webhook, which keeps its own admission/idempotency/quota gates)
	// is OBSERVATIONAL only — never re-launch or re-promote it.
	if v, ok := ev.Payload[PayloadLaunchedRunID]; ok && v != nil {
		return nil, nil
	}
	cands, err := subs.ListCandidates(ctx, ev)
	if err != nil {
		return nil, err
	}
	var out []Subscription
	for _, sub := range cands {
		if sub.Enabled && sub.Match.Match(ev) {
			out = append(out, sub)
		}
	}
	return out, nil
}

// effectOpts tunes applyEffect for its two callers: the bus path runs with
// the zero value; the outbox worker threads its persisted consume state
// through so a retry never re-spends a one-shot.
type effectOpts struct {
	// alreadyConsumed skips the one-shot label consume — an outbox retry
	// whose earlier attempt consumed and persisted the marker.
	alreadyConsumed bool
	// onConsumed, when non-nil, runs between the atomic label consume and
	// the launch (the outbox persists its ConsumeMarked row marker there).
	onConsumed func()
}

// applyEffect executes ONE (subscription, event) effect — the single effect
// body both delivery paths share. Error semantics: nil = executed;
// errEffectOneShotSpent = the one-shot was consumed by another event
// (terminal, not a failure); anything else = the effect did not happen (the
// bus path warns and moves on, the outbox path retries).
func (e *Evaluator) applyEffect(ctx context.Context, sub Subscription, ev Event, opts effectOpts) error {
	switch sub.EffectiveMode() {
	case bundle.ExecutionBoard:
		if e.board == nil {
			return fmt.Errorf("board-mode subscription %s but no board effect wired", sub.ID)
		}
		_, err := e.board.Promote(ctx, e.buildPlan(sub, ev))
		return err
	default:
		if e.launcher == nil {
			return fmt.Errorf("direct-mode subscription %s but no launcher wired", sub.ID)
		}
		if sub.ConsumeLabels && ev.Source == SourceBoard && !opts.alreadyConsumed {
			lc, ok := e.board.(LabelConsumer)
			if !ok {
				return fmt.Errorf("subscription %s requires consume_labels but the board effect cannot consume", sub.ID)
			}
			consumed, err := lc.ConsumeMatchLabels(ctx, ev.TenantID, ev.Subject.ID, sub.Match.Labels)
			if err != nil {
				return fmt.Errorf("consume labels: %w", err)
			}
			if !consumed {
				return errEffectOneShotSpent
			}
			if opts.onConsumed != nil {
				opts.onConsumed()
			}
		}
		_, err := e.launcher.Launch(ctx, e.buildPlan(sub, ev))
		return err
	}
}

// buildPlan resolves a (subscription, event) pair into a LaunchPlan: the
// subscription's static Vars, plus the event's free-text payload injected
// under ArgsVar (matching the dispatch_vars / webhook ArgsVar convention).
func (e *Evaluator) buildPlan(sub Subscription, ev Event) LaunchPlan {
	vars := make(map[string]string, len(sub.Vars)+2)
	// Per-event dynamic vars first (a forge/custom source stamps these under
	// payload["vars"]), then the subscription's static operator-pinned Vars on
	// top — operator pins win, mirroring the webhook LaunchVars precedence.
	for k, v := range eventVars(ev) {
		vars[k] = v
	}
	// A direct launch on a board event hands the bot its target card:
	// vars["issue_id"] is the card the bot operates ON (triage-style). The
	// promote path must NOT get it — it would pollute every promoted card's
	// BotArgs with a self-reference.
	if ev.Source == SourceBoard && sub.EffectiveMode() == bundle.ExecutionDirect && ev.Subject.ID != "" {
		vars["issue_id"] = ev.Subject.ID
	}
	for k, v := range sub.Vars {
		vars[k] = v
	}
	if sub.ArgsVar != "" {
		if payload := argsPayload(ev); payload != "" {
			vars[sub.ArgsVar] = payload
		}
	}
	return LaunchPlan{
		BotID:           sub.BotID,
		TenantID:        sub.TenantID,
		Repo:            firstNonEmpty(sub.Repo, ev.Repo),
		Mode:            sub.EffectiveMode(),
		Vars:            vars,
		KeyOverrides:    sub.KeyOverrides,
		SecretOverrides: sub.SecretOverrides,
		RepoURL:         ev.Subject.URL,
		RepoRef:         ev.Subject.Ref,
		Event:           ev,
		Retry:           sub.RetryPolicy(),
	}
}

// eventVars extracts per-event dynamic launch vars a source may stamp under
// payload["vars"] (a map[string]string or map[string]any with string values).
// Used by forge/custom sources to pass PR url / commit / arbitrary fields to
// the run; board/run/schedule sources typically carry none.
func eventVars(ev Event) map[string]string {
	raw, ok := ev.Payload[PayloadVars]
	if !ok {
		return nil
	}
	switch m := raw.(type) {
	case map[string]string:
		return m
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	return nil
}

// argsPayload derives the free-text payload an event injects under a
// subscription's ArgsVar: the subject title + body (the implementer-bot
// "feature_prompt" convention), falling back to whatever the source stamped
// under payload["args"].
func argsPayload(ev Event) string {
	title := strings.TrimSpace(ev.Subject.Title)
	body := strings.TrimSpace(ev.Subject.Body)
	switch {
	case title != "" && body != "":
		return title + "\n\n" + body
	case title != "":
		return title
	case body != "":
		return body
	}
	if a, ok := ev.Payload["args"].(string); ok {
		return strings.TrimSpace(a)
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (e *Evaluator) warn(format string, args ...any) {
	if e.logger != nil {
		e.logger.Warn(format, args...)
	}
}
