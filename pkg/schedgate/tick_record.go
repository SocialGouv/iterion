package schedgate

import "time"

// Surface names the scheduled-launch path that made a tick decision, so
// mixed audit streams can be filtered per-source.
type Surface string

const (
	SurfaceHostCron Surface = "host-cron"
	SurfaceTrigger  Surface = "trigger"
	SurfaceCloud    Surface = "cloud"
)

// TickDecision is the audit-level outcome of one tick. Exactly one tag
// lands on each record.
type TickDecision string

const (
	// TickFired: overlap + guard passed and a launch was attempted.
	TickFired TickDecision = "fired"
	// TickSkippedOverlap: a live run of the same schedule held the slot.
	TickSkippedOverlap TickDecision = "skipped_overlap"
	// TickGuardBlocked: the guard exited non-zero ("nothing to do").
	TickGuardBlocked TickDecision = "guard_blocked"
	// TickGuardError: the guard failed to execute (spawn error/timeout).
	TickGuardError TickDecision = "guard_error"
	// TickLaunchFailed: overlap + guard passed but the launch itself
	// errored. The slot is spent (at-most-once per occurrence; the next
	// cron slot is the retry) — recording it distinctly is what keeps a
	// "fired" audit from claiming a run that never started.
	TickLaunchFailed TickDecision = "launch_failed"
)

// TickRecord is the shared audit row written by every surface — as a
// JSONL line locally (~/.iterion/logs/tick-audit.jsonl), as audit.Event
// Meta in cloud mode. It answers "why didn't my scheduled bot fire last
// night?" deterministically. Field names are stable: operators grep the
// JSONL.
type TickRecord struct {
	// Schema is the record format version (currently 1). Readers must
	// ignore unknown fields and reject unknown higher schemas loudly.
	Schema int `json:"schema"`
	// Surface is the launch path that made the decision.
	Surface Surface `json:"surface"`
	// ScheduleID is the stable schedule identity: ScheduleEntry.Name
	// (host-cron), Subscription.ID (trigger), ScheduledBot.ID (cloud).
	ScheduleID string `json:"schedule_id"`
	// ScheduleName is the human label when it differs from the ID.
	ScheduleName string       `json:"schedule_name,omitempty"`
	BotID        string       `json:"bot_id,omitempty"`
	TenantID     string       `json:"tenant_id,omitempty"`
	Cron         string       `json:"cron,omitempty"`
	At           time.Time    `json:"at"`
	Decision     TickDecision `json:"decision"`
	// Reason is a human sentence, e.g. "blocked by live run r_x".
	Reason string `json:"reason,omitempty"`
	// RunID is the launched run on TickFired.
	RunID string `json:"run_id,omitempty"`
	// BlockingRunID names the (oldest) live run on TickSkippedOverlap so
	// the operator can inspect or cancel it.
	BlockingRunID string `json:"blocking_run_id,omitempty"`
	// GuardExit is the guard's exit code — pointer so 0 is meaningful.
	GuardExit     *int          `json:"guard_exit,omitempty"`
	GuardDuration time.Duration `json:"guard_duration_ns,omitempty"`
	StdoutTail    string        `json:"stdout_tail,omitempty"`
	StderrTail    string        `json:"stderr_tail,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// TickSchemaVersion is the current TickRecord schema.
const TickSchemaVersion = 1

// NewTickRecord builds a record with the invariant fields stamped.
func NewTickRecord(surface Surface, scheduleID string, at time.Time, decision TickDecision) TickRecord {
	return TickRecord{
		Schema:     TickSchemaVersion,
		Surface:    surface,
		ScheduleID: scheduleID,
		At:         at.UTC(),
		Decision:   decision,
	}
}

// ApplyGuard folds a guard result into the record (exit code, tails,
// duration, error text). The decision itself stays the caller's choice
// so a GuardOK record can still be TickFired.
func (t *TickRecord) ApplyGuard(res GuardResult) {
	if res.Kind != GuardError {
		code := res.ExitCode
		t.GuardExit = &code
	}
	t.GuardDuration = res.Duration
	if res.Kind != GuardOK {
		t.StdoutTail = TruncTail(res.Stdout, TailCap)
	}
	t.StderrTail = res.StderrTail
	if res.Err != nil {
		t.Error = res.Err.Error()
	}
}

// ToAuditMeta projects the record onto the map shape pkg/audit stores
// in Event.Meta. Kept here — not in each surface — so the cloud rows
// and the local JSONL stay field-compatible.
func (t TickRecord) ToAuditMeta() map[string]any {
	m := map[string]any{
		"schema":      t.Schema,
		"surface":     string(t.Surface),
		"schedule_id": t.ScheduleID,
		"decision":    string(t.Decision),
		"at":          t.At,
	}
	if t.ScheduleName != "" {
		m["schedule_name"] = t.ScheduleName
	}
	if t.BotID != "" {
		m["bot_id"] = t.BotID
	}
	if t.Cron != "" {
		m["cron"] = t.Cron
	}
	if t.Reason != "" {
		m["reason"] = t.Reason
	}
	if t.RunID != "" {
		m["run_id"] = t.RunID
	}
	if t.BlockingRunID != "" {
		m["blocking_run_id"] = t.BlockingRunID
	}
	if t.GuardExit != nil {
		m["guard_exit"] = *t.GuardExit
	}
	if t.GuardDuration != 0 {
		m["guard_duration_ms"] = t.GuardDuration.Milliseconds()
	}
	if t.StdoutTail != "" {
		m["stdout_tail"] = t.StdoutTail
	}
	if t.StderrTail != "" {
		m["stderr_tail"] = t.StderrTail
	}
	if t.Error != "" {
		m["error"] = t.Error
	}
	return m
}
