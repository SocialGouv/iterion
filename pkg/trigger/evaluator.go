package trigger

import (
	"context"
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
	cands, err := e.subs.ListCandidates(ctx, ev)
	if err != nil {
		return err
	}
	for _, sub := range cands {
		if !sub.Enabled || !sub.Match.Match(ev) {
			continue
		}
		plan := e.buildPlan(sub, ev)
		switch sub.EffectiveMode() {
		case bundle.ExecutionBoard:
			if e.board == nil {
				e.warn("trigger: subscription %s is board-mode but no board effect is wired; skipping", sub.ID)
				continue
			}
			if _, err := e.board.Promote(ctx, plan); err != nil {
				e.warn("trigger: promote for subscription %s failed: %v", sub.ID, err)
			}
		default:
			if e.launcher == nil {
				e.warn("trigger: subscription %s is direct-mode but no launcher is wired; skipping", sub.ID)
				continue
			}
			if _, err := e.launcher.Launch(ctx, plan); err != nil {
				e.warn("trigger: launch for subscription %s failed: %v", sub.ID, err)
			}
		}
	}
	return nil
}

// buildPlan resolves a (subscription, event) pair into a LaunchPlan: the
// subscription's static Vars, plus the event's free-text payload injected
// under ArgsVar (matching the dispatch_vars / webhook ArgsVar convention).
func (e *Evaluator) buildPlan(sub Subscription, ev Event) LaunchPlan {
	vars := make(map[string]string, len(sub.Vars)+1)
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
	}
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
