package trigger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// EffectWorker drains an EffectOutbox: claim → execute → mark, with bounded
// retries. Every replica may run one; the outbox's atomic claim is what keeps
// two replicas off the same row. Effects execute through the same Evaluator
// pieces (Launcher/BoardEffect) the bus path uses, so validation and
// semantics stay identical — the outbox changes WHEN an effect runs and what
// survives a failure, never what the effect does.
type EffectWorker struct {
	Outbox    EffectOutbox
	Subs      SubscriptionStore
	Evaluator *Evaluator
	Logger    *iterlog.Logger
	// Now is the clock seam (tests); nil → time.Now().UTC.
	Now func() time.Time
}

// errEffectOneShotSpent is the sentinel applyClaimedEffect returns when the
// one-shot was spent by another event — terminal success for THIS row.
var errEffectOneShotSpent = errors.New("trigger: one-shot already spent")

// Tick claims up to limit due rows and executes each. Returns the number of
// rows it acted on (tests + callers pacing on activity).
func (w *EffectWorker) Tick(ctx context.Context, limit int) int {
	now := w.now()
	rows, err := w.Outbox.ClaimDue(ctx, now, limit)
	if err != nil {
		w.warn("trigger: effect outbox claim: %v", err)
		return 0
	}
	for i := range rows {
		w.executeOne(ctx, &rows[i])
	}
	return len(rows)
}

func (w *EffectWorker) executeOne(ctx context.Context, row *EffectRow) {
	err := w.applyClaimedEffect(ctx, row)
	switch {
	case err == nil, errors.Is(err, errEffectOneShotSpent):
		if merr := w.Outbox.MarkDone(ctx, row.ID); merr != nil {
			// The effect ran; a failed done-write means one redundant retry
			// of an idempotent/one-shot-guarded effect, not a loss.
			w.warn("trigger: effect %s done-write failed (will re-run idempotently): %v", row.ID, merr)
		}
	default:
		attempts := row.Attempts + 1
		if attempts >= MaxEffectAttempts {
			w.warn("trigger: effect %s (sub %s, event %s) FAILED after %d attempts — parked as dead-letter: %v",
				row.ID, row.SubID, row.Event.ID, attempts, err)
			if merr := w.Outbox.MarkFailed(ctx, row.ID, err.Error()); merr != nil {
				w.warn("trigger: effect %s failed-write: %v", row.ID, merr)
			}
			return
		}
		backoff := EffectBackoff(attempts)
		w.warn("trigger: effect %s (sub %s) attempt %d/%d failed, retrying in %s: %v",
			row.ID, row.SubID, attempts, MaxEffectAttempts, backoff, err)
		if merr := w.Outbox.MarkRetry(ctx, row.ID, attempts, w.now().Add(backoff), err.Error()); merr != nil {
			w.warn("trigger: effect %s retry-write: %v", row.ID, merr)
		}
	}
}

// applyClaimedEffect runs one (subscription, event) effect under the row's
// claim. Error semantics: nil = executed; errEffectOneShotSpent = the one-shot was
// consumed by another event (terminal for this row); anything else = retry.
func (w *EffectWorker) applyClaimedEffect(ctx context.Context, row *EffectRow) error {
	sub, err := w.Subs.Get(ctx, row.SubID)
	if err != nil {
		// A deleted subscription is terminal, not retryable: the operator
		// removed the binding between materialization and execution.
		w.warn("trigger: effect %s: subscription %s gone — dropping: %v", row.ID, row.SubID, err)
		return nil
	}
	if !sub.Enabled {
		return nil // disabled between materialization and execution — operator's call
	}
	e := w.Evaluator
	ev := row.Event
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
		if sub.ConsumeLabels && ev.Source == SourceBoard && !row.ConsumeMarked {
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
			// Persist "the one-shot is OURS" BEFORE launching: a launch
			// failure (or a crash) then retries the launch WITHOUT
			// re-consuming — the pre-outbox shape lost the trigger here.
			if err := w.Outbox.MarkConsumed(ctx, row.ID); err != nil {
				// The consume happened; without the marker a reclaim would
				// read "spent by another event" and drop the launch. Press
				// on to the launch NOW — this attempt is the marker.
				w.warn("trigger: effect %s consumed but marker write failed (%v) — launching in-line to not lose the one-shot", row.ID, err)
			}
			row.ConsumeMarked = true
		}
		_, err := e.launcher.Launch(ctx, e.buildPlan(sub, ev))
		return err
	}
}

func (w *EffectWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

func (w *EffectWorker) warn(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Warn(format, args...)
	}
}

// MaterializeEffects computes the outbox rows one normalized event owes: one
// row per enabled, matching, non-observational subscription. Shared by the
// cloud board source (the writer) and tests. now stamps CreatedAt.
func MaterializeEffects(ctx context.Context, subs SubscriptionStore, ev Event, now time.Time) ([]EffectRow, error) {
	if v, ok := ev.Payload[PayloadLaunchedRunID]; ok && v != nil {
		return nil, nil // already launched by an authoritative path — observational
	}
	cands, err := subs.ListCandidates(ctx, ev)
	if err != nil {
		return nil, err
	}
	var rows []EffectRow
	for _, sub := range cands {
		if !sub.Enabled || !sub.Match.Match(ev) {
			continue
		}
		rows = append(rows, EffectRow{
			ID:        EffectID(ev.ID, sub.ID),
			TenantID:  ev.TenantID,
			Event:     ev,
			SubID:     sub.ID,
			State:     EffectPending,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	return rows, nil
}
