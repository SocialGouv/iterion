package cloudsched

import (
	"context"
	"errors"
	"strings"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// LaunchFunc fires one scheduled bot run. The cloud bootstrap wires it to the
// run publisher (resolve the bot, build a LaunchSpec, SubmitLaunch).
type LaunchFunc func(ctx context.Context, sb ScheduledBot) error

// GateFunc decides, AFTER this replica claimed the slot's intention, whether the
// launch proceeds (overlap policy + guard, pkg/schedgate). On
// proceed=false, rec is the skip record for the audit sink; on
// proceed=true rec is ignored (the ticker audits the fired outcome
// itself, with the launch error) and guardStdout carries the guard's
// stdout ("" when no guard) for injection into the launch vars.
// Only the intent claimant runs the guard; its admitted payload is persisted
// before launch so a maintenance retry does not consume the guard again.
// Prepared intentions call Gate again with Guard empty: the overlap verdict
// must reflect runs launched since preparation, while guard vars stay intact.
type GateFunc func(ctx context.Context, sb ScheduledBot) (proceed bool, guardStdout string, rec schedgate.TickRecord)

// Ticker materializes each due slot before advancing its calendar, then
// executes leased intentions. A hold keeps the same intention pending.
// Claims prevent concurrent execution; a crash after launch but before the
// completion write can still cause an at-least-once replay.
type Ticker struct {
	Store    Store
	Launch   LaunchFunc
	Interval time.Duration // default 1 minute
	Logger   *iterlog.Logger
	// Gate, when set, runs the overlap/guard policy between the CAS win
	// and the launch. Audit, when set, receives every gate decision AND
	// the fired outcome. Both nil-safe (nil = fire unconditionally,
	// unaudited — the pre-gate behavior).
	Gate  GateFunc
	Audit func(rec schedgate.TickRecord)
	// Now is injectable for tests; defaults to time.Now().UTC().
	Now func() time.Time
}

func (t *Ticker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

// Tick fires every due schedule this caller wins the CAS for, returning the
// count it fired. Exposed for tests + a manual kick.
func (t *Ticker) Tick(ctx context.Context) (int, error) {
	now := t.now()
	due, err := t.Store.ListDue(ctx, now, 200)
	if err != nil {
		return 0, err
	}
	fired := 0
	for _, sb := range due {
		next, nerr := NextFireForBot(sb, now)
		if nerr != nil {
			t.warn("bad schedule on %s: %v", sb.ID, nerr)
			continue
		}
		if err := t.Store.PutIntent(ctx, sb); err != nil {
			t.warn("materialize slot %s: %v", sb.ID, err)
			continue
		}
		_, cerr := t.Store.ClaimTick(ctx, sb.ID, sb.NextFireAt, next, now)
		if cerr != nil {
			t.warn("claim %s: %v", sb.ID, cerr)
			continue
		}
	}
	intents, err := t.Store.ClaimIntents(ctx, now, 200)
	if err != nil {
		return fired, err
	}
	for _, intent := range intents {
		if t.now().After(intent.NotBefore) {
			continue
		}
		sb := intent.Schedule
		current, err := t.Store.Get(ctx, sb.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			continue
		} // lease recovers transient reads
		finish := func(hold bool) {
			if err := t.Store.FinishIntent(ctx, intent, hold, now); err != nil {
				t.warn("finish intent %s: %v", intent.ID, err)
			}
		}
		if errors.Is(err, ErrNotFound) || current.Disabled {
			finish(false)
			continue
		}
		fired++
		if t.Gate != nil {
			gateSchedule := sb
			if intent.Prepared {
				gateSchedule.Guard = ""
			}
			proceed, guardStdout, rec := t.Gate(ctx, gateSchedule)
			if !proceed {
				if t.Audit != nil {
					t.Audit(rec)
				}
				// A guard that could not RUN stops the schedule producing
				// runs, which its operator reads on the schedule itself
				// (last_error), not in the audit trail. A guard that ran and
				// said "nothing to do" is the schedule working: it raises
				// nothing. Either way the verdict replaces the previous one,
				// so a schedule that recovers is no longer marked unhealthy.
				t.recordGateVerdict(ctx, current, rec, now)
				finish(false)
				t.warn("gate skipped %s (%s): %s", sb.ID, sb.BotID, rec.Reason)
				continue
			}
			if !intent.Prepared && guardStdout != "" {
				// Copy-on-write: sb is a loop copy, but Vars is a shared map.
				vars := make(map[string]string, len(sb.Vars)+1)
				for k, v := range sb.Vars {
					vars[k] = v
				}
				vars[sb.Policy().GuardVar] = guardStdout
				sb.Vars = vars
			}
		}
		if !intent.Prepared {
			intent.Schedule = sb
			prepared, err := t.Store.PrepareIntent(ctx, intent)
			if err != nil {
				t.warn("prepare intent %s: %v", intent.ID, err)
			}
			if err != nil || !prepared {
				continue
			}
		}
		// Maintenance postpones this intention. Ordinary failures retain
		// the scheduler's historical terminal verdict for this slot.
		lerr := t.Launch(ctx, sb)
		finish(errors.Is(lerr, schedgate.ErrMaintenanceHold))
		if lerr != nil {
			t.warn("launch %s (%s): %v", sb.ID, sb.BotID, lerr)
		}
		t.recordLaunchVerdict(ctx, current, lerr, now)
		if t.Audit != nil {
			rec := schedgate.NewTickRecord(schedgate.SurfaceCloud, sb.ID, now, schedgate.LaunchDecision(lerr))
			rec.ScheduleName = sb.BotID
			rec.BotID = sb.BotID
			rec.TenantID = sb.TenantID
			rec.Cron = sb.Cron
			if lerr != nil {
				rec.Error = lerr.Error()
			}
			t.Audit(rec)
		}
	}
	return fired, nil
}

// recordGateVerdict persists on the schedule's own health field what the gate
// decided: a guard that failed to EXECUTE — refused by the deployment, or
// unable to spawn — produces no run and no launch verdict, so without this
// the schedule reads as healthy while nothing fires; and a guard that ran
// clears it again. A skipped overlap says nothing: the schedule is busy, not
// unhealthy.
func (t *Ticker) recordGateVerdict(ctx context.Context, sb ScheduledBot, rec schedgate.TickRecord, now time.Time) {
	switch rec.Decision {
	case schedgate.TickGuardError:
		msg := strings.TrimSpace(rec.Reason)
		if msg == "" {
			msg = string(rec.Decision)
		}
		t.markLaunchError(ctx, sb, msg, now)
	case schedgate.TickGuardBlocked:
		// The guard RAN and said "nothing to do": the schedule is working,
		// whatever the previous verdict was — a refused guard, a launch the
		// org gate denied. The history stays in the audit trail; this field
		// answers "is it working now?", and a stale error here reads as a
		// schedule that stopped, which is the opposite of what happened.
		t.markLaunchError(ctx, sb, "", now)
	}
}

// recordLaunchVerdict persists on the schedule itself what became of the tick
// that just consumed its slot: a refusal — an org launch-gate denial above all
// — raises last_error, and the next tick that launches clears it. Written only
// when the verdict CHANGES, so a schedule that has been refused for a week
// costs one write, not one per minute. A failed write is logged, never fatal:
// the slot is already consumed and the audit row still carries the error.
func (t *Ticker) recordLaunchVerdict(ctx context.Context, sb ScheduledBot, launchErr error, now time.Time) {
	msg := ""
	if launchErr != nil {
		msg = launchErr.Error()
	}
	t.markLaunchError(ctx, sb, msg, now)
}

// markLaunchError writes msg as the schedule's health, only when it CHANGES:
// a schedule refused for a week costs one write, not one per minute. A failed
// write is logged, never fatal — the slot is already consumed and the audit
// row still carries the reason.
func (t *Ticker) markLaunchError(ctx context.Context, sb ScheduledBot, msg string, now time.Time) {
	if msg == sb.LastError {
		return
	}
	if err := t.Store.MarkLaunchError(ctx, sb.ID, msg, now); err != nil {
		t.warn("recording the verdict of %s: %v", sb.ID, err)
	}
}

// Run loops Tick every Interval until ctx is cancelled. Start one per replica.
func (t *Ticker) Run(ctx context.Context) {
	interval := t.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		if _, err := t.Tick(ctx); err != nil {
			t.warn("tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

func (t *Ticker) warn(format string, args ...any) {
	if t.Logger != nil {
		t.Logger.Warn("cloudsched: "+format, args...)
	}
}
