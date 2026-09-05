package trigger

import (
	"context"
	"errors"
	"fmt"
	"time"

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
		row := &rows[i]
		// A row whose reclaim already spent the attempt budget parks as a
		// dead-letter HERE — an effect that never returns (hung worker,
		// killed pod) never reaches MarkRetry, so without this check the
		// MaxEffectAttempts guard would be unreachable for it.
		if row.Attempts >= MaxEffectAttempts {
			w.warn("trigger: effect %s (sub %s) reclaimed %d times without completing — parked as dead-letter", row.ID, row.SubID, row.Attempts)
			if merr := w.Outbox.MarkFailed(ctx, row.ID, row.ClaimID, "reclaimed past the attempt budget without completing"); merr != nil {
				w.warn("trigger: effect %s failed-write: %v", row.ID, merr)
			}
			continue
		}
		// The batch shares one claim instant; if executing the earlier rows
		// outlived this row's lease, another worker may already own it —
		// executing it here would double the effect with no crash involved.
		if w.now().After(row.NotBefore) {
			continue // lease expired before we reached it — reclaimable, not ours
		}
		w.executeOne(ctx, row)
	}
	return len(rows)
}

func (w *EffectWorker) executeOne(ctx context.Context, row *EffectRow) {
	err := w.applyClaimedEffect(ctx, row)
	switch {
	case err == nil, errors.Is(err, errEffectOneShotSpent), errors.Is(err, errEffectMachineCaused):
		if merr := w.Outbox.MarkDone(ctx, row.ID, row.ClaimID); merr != nil {
			// The effect ran; a failed done-write means one redundant retry
			// of an idempotent/one-shot-guarded effect, not a loss.
			w.warn("trigger: effect %s done-write failed (will re-run idempotently): %v", row.ID, merr)
		}
	default:
		attempts := row.Attempts + 1
		if attempts >= MaxEffectAttempts {
			w.warn("trigger: effect %s (sub %s, event %s) FAILED after %d attempts — parked as dead-letter: %v",
				row.ID, row.SubID, row.Event.ID, attempts, err)
			if merr := w.Outbox.MarkFailed(ctx, row.ID, row.ClaimID, err.Error()); merr != nil {
				w.warn("trigger: effect %s failed-write: %v", row.ID, merr)
			}
			return
		}
		backoff := EffectBackoff(attempts)
		w.warn("trigger: effect %s (sub %s) attempt %d/%d failed, retrying in %s: %v",
			row.ID, row.SubID, attempts, MaxEffectAttempts, backoff, err)
		if merr := w.Outbox.MarkRetry(ctx, row.ID, row.ClaimID, attempts, w.now().Add(backoff), err.Error()); merr != nil {
			w.warn("trigger: effect %s retry-write: %v", row.ID, merr)
		}
	}
}

// applyClaimedEffect runs one (subscription, event) effect under the row's
// claim, through the SAME effect body the bus path uses
// (Evaluator.applyEffect) — the worker only contributes the persisted
// consume state: alreadyConsumed skips a re-spend on retry, onConsumed
// persists "the one-shot is OURS" between the atomic consume and the launch
// (the pre-outbox shape lost the trigger exactly there).
func (w *EffectWorker) applyClaimedEffect(ctx context.Context, row *EffectRow) error {
	if row.IsProjection() {
		// No subscription to load, re-verify or tenant-check: a projection row
		// is owed to the tenant's board BINDING. That is also why it can never
		// be listed or deleted through /api/v1/triggers — it names no
		// subscription at all. The row's own tenant is on the event, which the
		// reflect resolves the binding from.
		return w.Evaluator.applyEffect(ctx, Subscription{}, row.Event, effectOpts{kind: EffectKindProjection})
	}
	sub, err := w.Subs.Get(ctx, row.SubID)
	switch {
	case errors.Is(err, ErrSubscriptionNotFound):
		// A DELETED subscription is terminal, not retryable: the operator
		// removed the binding between materialization and execution.
		w.warn("trigger: effect %s: subscription %s gone — dropping: %v", row.ID, row.SubID, err)
		return nil
	case err != nil:
		// A transient store failure is NOT a deletion — the same
		// transient≠definitive distinction NormalizeBoardEvent draws, one
		// seam lower. Retry.
		return fmt.Errorf("load subscription %s: %w", row.SubID, err)
	}
	if !sub.Enabled {
		return nil // disabled between materialization and execution — operator's call
	}
	// Cross-tenant defence in depth: a row must never execute under another
	// tenant's subscription (plan.TenantID drives which board it writes to).
	if sub.TenantID != row.TenantID {
		w.warn("trigger: effect %s: subscription %s belongs to tenant %q, row to %q — dropping", row.ID, sub.ID, sub.TenantID, row.TenantID)
		return nil
	}
	// Re-verify ADMISSION at execution time: the subscription may have been
	// re-pointed/re-scoped between materialization and now (the window is
	// the retry backoff, up to minutes) — the CURRENT rule decides, never a
	// stale match.
	if !sub.Match.Match(row.Event) {
		w.warn("trigger: effect %s: event no longer matches subscription %s (edited since materialization) — dropping", row.ID, sub.ID)
		return nil
	}
	return w.Evaluator.applyEffect(ctx, sub, row.Event, effectOpts{
		alreadyConsumed: row.ConsumeMarked,
		onConsumed: func() {
			if err := w.Outbox.MarkConsumed(ctx, row.ID, row.ClaimID); err != nil {
				// The consume happened; without the marker a reclaim would
				// read "spent by another event" and drop the launch. Press
				// on to the launch NOW — this attempt is the marker.
				w.warn("trigger: effect %s consumed but marker write failed (%v) — launching in-line to not lose the one-shot", row.ID, err)
			}
		},
	})
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

// ProjectionBindings answers the ONE question the projection arm asks: does
// this tenant have an external board bound? Narrow on purpose — pkg/trigger
// must not learn the forge binding's shape to know that a projection is owed,
// and the same implementation supplies the ProjectionEffect that executes it,
// so the pair cannot be half-wired.
type ProjectionBindings interface {
	HasBoardBinding(ctx context.Context, tenantID string) (bool, error)
}

// MaterializeEffects computes the outbox rows one normalized event owes: one
// row per enabled, matching, non-observational subscription, plus — for a card
// MOVE on a tenant with a bound external board — one projection row. Shared by
// the cloud board source (the writer) and tests. now stamps CreatedAt.
//
// bindings may be nil (a deployment with no board binding at all), in which
// case no projection row is ever owed.
func MaterializeEffects(ctx context.Context, subs SubscriptionStore, bindings ProjectionBindings, ev Event, now time.Time) ([]EffectRow, error) {
	var rows []EffectRow
	// The projection is computed BEFORE — and outside of —
	// matchingSubscriptions, deliberately: that prelude declines
	// machine-caused events to protect the fleet from mass LAUNCHES, and a
	// watchdog filing a card in `blocked` is exactly what the external
	// roadmap must show. A projection spends no budget and starts no run.
	owed, err := projectionOwed(ctx, bindings, ev)
	if err != nil {
		return nil, err
	}
	if owed {
		rows = append(rows, EffectRow{
			ID:        ProjectionEffectID(ev.ID),
			TenantID:  ev.TenantID,
			Event:     ev,
			Kind:      EffectKindProjection,
			State:     EffectPending,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	matched, err := matchingSubscriptions(ctx, subs, ev)
	if err != nil {
		return nil, err
	}
	for _, sub := range matched {
		rows = append(rows, EffectRow{
			ID:        EffectID(ev.ID, sub.ID),
			TenantID:  ev.TenantID,
			Event:     ev,
			Kind:      EffectKindLaunch,
			SubID:     sub.ID,
			State:     EffectPending,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	return rows, nil
}

// projectionOwed reports whether this event owes the tenant's bound board a
// state push. A binding lookup failure is RETURNED, never swallowed: the cloud
// tail materializes before advancing its cursor, so a skipped projection here
// is a reflect that only the periodic pass would ever make good.
func projectionOwed(ctx context.Context, bindings ProjectionBindings, ev Event) (bool, error) {
	// Only a state transition moves a column, and only a card the reflect can
	// name, for a tenant a binding can be resolved for.
	if bindings == nil || ev.Source != SourceBoard || ev.Kind != KindCardMoved ||
		ev.Subject.ID == "" || ev.TenantID == "" {
		return false, nil
	}
	bound, err := bindings.HasBoardBinding(ctx, ev.TenantID)
	if err != nil {
		return false, fmt.Errorf("board binding for tenant %q: %w", ev.TenantID, err)
	}
	return bound, nil
}
