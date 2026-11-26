package cloudsched

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// StallCause names which of the schedule row's own health fields shows the
// stall.
type StallCause string

const (
	// StallOverdue: next_fire_at, and the row's last edit (updated_at), lie
	// more than the grace in the past, so no ticker has claimed the slot —
	// the scheduler is dead, stuck, or its store writes fail. No run and no
	// launch verdict exist to say so.
	StallOverdue StallCause = "overdue"
	// StallLaunchRefused: the last tick that consumed or held a slot
	// launched nothing (last_error) — an org launch-gate denial, a refused
	// run, a guard that could not execute, an unreadable fence record, a run
	// that could not be persisted. No run exists, so no run alert fires.
	StallLaunchRefused StallCause = "launch_refused"
	// StallRunFailed: the last run the schedule launched ended failed, or
	// parked failed_resumable (last_run_status).
	StallRunFailed StallCause = "run_failed"
)

// ScheduleStall is the verdict of AssessScheduleStall on an unhealthy row.
type ScheduleStall struct {
	Cause StallCause
	// Reason is the operator-facing sentence: the bot, its cadence and the
	// field that shows the stall.
	Reason string
	// Episode identifies this stall episode on this row. It is stable while
	// the row keeps showing the same stall, so an alert keyed on it goes out
	// once per episode, and it changes when the row shows a new one (another
	// failed run, a refusal raised again after a clear, another stuck slot).
	Episode string
}

// AssessScheduleStall evaluates the schedule row's own health fields and
// returns nil for a healthy row. A disabled schedule is never stalled: its
// operator paused it.
//
// The causes are checked upstream first — overdue (nothing fires), then a
// refused launch (fires, no run), then a failed run (a run, failed) — so a
// live upstream stall is never masked by an older downstream verdict that
// is still on the row.
//
// Each cause clears from the row itself — the ticker advances next_fire_at
// on its next claim, the next tick that launches clears last_error, the
// next run that finishes or is cancelled overwrites last_run_status — so a
// recovered schedule assesses healthy without any state kept here.
func AssessScheduleStall(sb ScheduledBot, now time.Time, grace time.Duration) *ScheduleStall {
	if sb.Disabled {
		return nil
	}
	cadence := sb.Cron
	if sb.IntervalSeconds > 0 {
		cadence = fmt.Sprintf("every %ds", sb.IntervalSeconds)
	}
	// The grace runs from the later of the slot and the row's last edit: a
	// resume or an edit re-arms the row, and the stale slot it carries is
	// the ticker's to catch up on its next tick, not a dead scheduler.
	overdueFrom := sb.NextFireAt
	if sb.UpdatedAt.After(overdueFrom) {
		overdueFrom = sb.UpdatedAt
	}
	if !sb.NextFireAt.IsZero() && now.Sub(overdueFrom) > grace {
		return &ScheduleStall{
			Cause: StallOverdue,
			Reason: fmt.Sprintf("schedule %s (bot %s, %s) is overdue: next_fire_at %s is %s in the past and no scheduler claimed the slot",
				sb.ID, sb.BotID, cadence, sb.NextFireAt.UTC().Format(time.RFC3339), now.Sub(sb.NextFireAt).Truncate(time.Minute)),
			Episode: fmt.Sprintf("overdue:%d", sb.NextFireAt.UnixMilli()),
		}
	}
	if sb.LastError != "" {
		since := "unknown time"
		// last_error_at is written with last_error by both store twins; the
		// message fingerprint identifies the episode when it is absent.
		anchor := "e:" + fingerprint(sb.LastError)
		if sb.LastErrorAt != nil {
			since = sb.LastErrorAt.UTC().Format(time.RFC3339)
			anchor = fmt.Sprintf("%d", sb.LastErrorAt.UnixMilli())
		}
		return &ScheduleStall{
			Cause: StallLaunchRefused,
			Reason: fmt.Sprintf("schedule %s (bot %s, %s) launches no run since %s: last_error: %s",
				sb.ID, sb.BotID, cadence, since, truncateReason(sb.LastError)),
			Episode: "launch:" + anchor,
		}
	}
	// failed_resumable is named, not read through IsTerminalResumable: that
	// predicate also holds for cancelled, and an operator cancel is a
	// decision, not a stall.
	status := store.RunStatus(sb.LastRunStatus)
	if sb.LastRunID != "" && (status.IsFinalFailure() || status == store.RunStatusFailedResumable) {
		detail := sb.LastRunError
		if sb.LastRunErrorCode != "" {
			detail = fmt.Sprintf("[%s] %s", sb.LastRunErrorCode, detail)
		}
		return &ScheduleStall{
			Cause: StallRunFailed,
			Reason: fmt.Sprintf("schedule %s (bot %s, %s): last run %s ended %s: %s",
				sb.ID, sb.BotID, cadence, sb.LastRunID, sb.LastRunStatus, truncateReason(detail)),
			Episode: "run:" + sb.LastRunID + ":" + sb.LastRunStatus,
		}
	}
	return nil
}

func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// truncateReason bounds an error excerpt carried into an alert.
func truncateReason(s string) string {
	const limit = 300
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
