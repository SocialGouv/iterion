package cloudsched

import (
	"context"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// LaunchFunc fires one scheduled bot run. The cloud bootstrap wires it to the
// run publisher (resolve the bot, build a LaunchSpec, SubmitLaunch).
type LaunchFunc func(ctx context.Context, sb ScheduledBot) error

// GateFunc decides, AFTER this replica won the slot's CAS, whether the
// launch proceeds (overlap policy + guard, pkg/schedgate). On
// proceed=false, rec is the skip record for the audit sink; on
// proceed=true rec is ignored (the ticker audits the fired outcome
// itself, with the launch error) and guardStdout carries the guard's
// stdout ("" when no guard) for injection into the launch vars.
// Positioned after the CAS on purpose: the slot must be consumed
// exactly once across replicas regardless of the gate's verdict, and
// only the CAS winner may run the guard (a pre-CAS gate would run it
// on every replica).
type GateFunc func(ctx context.Context, sb ScheduledBot) (proceed bool, guardStdout string, rec schedgate.TickRecord)

// Ticker fires due schedules. It is multi-replica-safe WITHOUT leader
// election: every replica may run a Ticker; the CAS in ClaimTick guarantees
// each slot fires exactly once (the first replica to advance next_fire_at
// wins; the rest see the moved value and skip).
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
		won, cerr := t.Store.ClaimTick(ctx, sb.ID, sb.NextFireAt, next, now)
		if cerr != nil {
			t.warn("claim %s: %v", sb.ID, cerr)
			continue
		}
		if !won {
			continue // another replica claimed this slot
		}
		// The slot is already advanced (consumed) whatever happens next —
		// `fired` counts slot activity, not successful launches.
		fired++
		if t.Gate != nil {
			proceed, guardStdout, rec := t.Gate(ctx, sb)
			if !proceed {
				if t.Audit != nil {
					t.Audit(rec)
				}
				t.warn("gate skipped %s (%s): %s", sb.ID, sb.BotID, rec.Reason)
				continue
			}
			if guardStdout != "" {
				// Copy-on-write: sb is a loop copy, but Vars is a shared map.
				vars := make(map[string]string, len(sb.Vars)+1)
				for k, v := range sb.Vars {
					vars[k] = v
				}
				vars[sb.Policy().GuardVar] = guardStdout
				sb.Vars = vars
			}
		}
		// A failed launch is logged, not retried within the slot (the
		// next slot fires normally). This is at-most-once-per-slot,
		// matching the host-crontab scheduler.
		lerr := t.Launch(ctx, sb)
		if lerr != nil {
			t.warn("launch %s (%s): %v", sb.ID, sb.BotID, lerr)
		}
		if t.Audit != nil {
			rec := schedgate.NewTickRecord(schedgate.SurfaceCloud, sb.ID, now, schedgate.TickFired)
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
