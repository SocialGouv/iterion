package schedgate

import (
	"context"
	"fmt"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// GateInput bundles what a launch surface knows at tick time. The
// three scheduled-launch surfaces (host-cron, trigger spine, cloud
// ticker) all evaluate the SAME overlap→guard sequence; only the
// audit sink, the human reporting, and the guard's cwd/env differ —
// so those stay with the caller and everything else lives in Apply.
type GateInput struct {
	Policy Policy
	// Lister powers the overlap check; nil skips it (a surface with no
	// store access degrades to guard-only, never to a hard error).
	Lister ScheduleRunLister
	// ScheduleID is the provenance key runs were stamped with
	// (RunSource.ScheduleID) — what LiveRunsForSchedule queries.
	ScheduleID string
	// Record is the surface-prefilled tick record (surface, ids,
	// tenant, cron, timestamp). Apply stamps the decision, reason and
	// guard fields on the copy it returns.
	Record TickRecord
	// GuardDir/GuardEnv shape the guard subprocess (see GuardSpec).
	GuardDir string
	GuardEnv []string
	Logger   *iterlog.Logger
	// Now overrides the clock for keepalive staleness detection (zero =
	// time.Now()). Tests inject a fixed instant; production leaves it zero.
	Now time.Time
}

// GateOutcome is the gate's verdict for one consumed tick slot.
type GateOutcome struct {
	// Proceed: launch the run. False: the slot passes, and Record
	// carries the audited reason (skipped_overlap / guard_blocked /
	// guard_error).
	Proceed bool
	// GuardRan is true when a guard command executed and passed —
	// callers then inject GuardStdout as vars[Policy.GuardVar], even
	// when the stdout is empty (the var's presence is the contract).
	GuardRan    bool
	GuardStdout string
	// Record is the input record stamped with the decision. On
	// Proceed it is left undecided — "fired" is only true after the
	// launch attempt, which the caller owns.
	Record TickRecord
	// ReapRunIDs lists keepalive runs found stale (silent past
	// StaleAfter) at this tick. The caller cancels them via its store so
	// the zombies free resources; schedgate stays I/O-free. Non-empty
	// only on the keepalive path, and only alongside Proceed (a stale
	// run no longer blocks, so the tick fires a fresh one).
	ReapRunIDs []string
}

// Apply runs the shared overlap→guard gate sequence. It never returns
// an error: a broken guard is a first-class audited outcome
// (guard_error), not a launch-path failure.
func Apply(ctx context.Context, in GateInput) GateOutcome {
	policy := Normalize(in.Policy)

	var reap []string
	if in.Lister != nil {
		var live []string
		if policy.Overlap == OverlapKeepalive {
			live, reap = LiveAndStaleRunsForSchedule(ctx, in.Lister, in.ScheduleID, policy.StaleAfterDuration(), in.Now, in.Logger)
		} else {
			live = LiveRunsForSchedule(ctx, in.Lister, in.ScheduleID, in.Logger)
		}
		if decision, blocking := EvaluateOverlap(live, policy); decision == DecisionSkipOverlap {
			rec := in.Record
			rec.Decision = TickSkippedOverlap
			rec.BlockingRunID = blocking
			rec.Reason = fmt.Sprintf("blocked by live run %s (%d live, overlap=%s)", blocking, len(live), policy.Overlap)
			return GateOutcome{Record: rec}
		}
	}

	if policy.Guard == "" {
		return GateOutcome{Proceed: true, Record: in.Record, ReapRunIDs: reap}
	}

	res := RunGuard(ctx, GuardSpec{
		Command: policy.Guard,
		Dir:     in.GuardDir,
		Env:     in.GuardEnv,
		Timeout: policy.GuardTimeoutDuration(),
	})
	switch res.Kind {
	case GuardBlocked:
		rec := in.Record
		rec.Decision = TickGuardBlocked
		rec.Reason = fmt.Sprintf("guard exited %d — nothing to do", res.ExitCode)
		rec.ApplyGuard(res)
		return GateOutcome{Record: rec}
	case GuardError:
		rec := in.Record
		rec.Decision = TickGuardError
		rec.Reason = "guard failed to execute"
		rec.ApplyGuard(res)
		return GateOutcome{Record: rec}
	default:
		return GateOutcome{Proceed: true, GuardRan: true, GuardStdout: res.Stdout, Record: in.Record, ReapRunIDs: reap}
	}
}
