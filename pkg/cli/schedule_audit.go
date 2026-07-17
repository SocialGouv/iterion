package cli

import (
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/jsonl"
	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// ---------------------------------------------------------------------------
// Tick audit — the durable "why didn't my scheduled bot fire?" trail
//
// Every RunScheduleRun decision (fired / skipped_overlap / guard_blocked /
// guard_error) appends one schedgate.TickRecord to
// <manifest-dir>/logs/tick-audit.jsonl, next to the per-schedule cron
// logs. `iterion schedule audit` reads it back.
// ---------------------------------------------------------------------------

// tickAuditPath places the audit log alongside the cron output logs
// (same directory renderCronBlock points the per-schedule logs at).
// The path shape lives in schedgate so the in-process trigger
// scheduler writes to the same file.
func tickAuditPath(manifestPath string) string {
	return schedgate.LocalAuditPathFor(manifestPath)
}

// newHostCronTickRecord stamps the invariant host-cron fields.
func newHostCronTickRecord(e ScheduleEntry, decision schedgate.TickDecision) schedgate.TickRecord {
	rec := schedgate.NewTickRecord(schedgate.SurfaceHostCron, e.Name, time.Now(), decision)
	rec.ScheduleName = e.Name
	rec.BotID = e.Bot
	rec.Cron = e.Cron
	return rec
}

// writeTickAudit appends the record best-effort: the audit must never
// turn a successful (or deliberately skipped) tick into a cron failure,
// so an append error is surfaced on stderr-equivalent output only.
func writeTickAudit(p *Printer, path string, rec schedgate.TickRecord) {
	if err := jsonl.AppendJSON(path, rec); err != nil {
		p.Line("⚠ tick audit append failed (%s): %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// iterion schedule audit
// ---------------------------------------------------------------------------

type ScheduleAuditOptions struct {
	ScheduleCommonOptions
	Name    string // filter: schedule name/id ("" = all)
	Surface string // filter: host-cron|trigger|cloud ("" = all)
	Since   string // filter: Go duration lookback ("" = all)
	Tail    int    // keep only the last N matching rows (0 = all)
}

// RunScheduleAudit prints the tick-decision history recorded by
// RunScheduleRun (and any other surface writing to the same file).
func RunScheduleAudit(p *Printer, opts ScheduleAuditOptions) error {
	path, err := resolveScheduleManifestPath(opts.ManifestPath)
	if err != nil {
		return err
	}
	auditPath := tickAuditPath(path)

	records, err := jsonl.ReadLines[schedgate.TickRecord](auditPath)
	if err != nil {
		return err
	}

	var since time.Time
	if opts.Since != "" {
		d, err := time.ParseDuration(opts.Since)
		if err != nil {
			return fmt.Errorf("invalid --since %q: %w", opts.Since, err)
		}
		since = time.Now().Add(-d)
	}

	filtered := records[:0:0]
	for _, r := range records {
		if r.Schema > schedgate.TickSchemaVersion {
			p.Line("⚠ skipping tick record with unknown schema %d (upgrade iterion to read it)", r.Schema)
			continue
		}
		if opts.Name != "" && r.ScheduleID != opts.Name && r.ScheduleName != opts.Name {
			continue
		}
		if opts.Surface != "" && string(r.Surface) != opts.Surface {
			continue
		}
		if !since.IsZero() && r.At.Before(since) {
			continue
		}
		filtered = append(filtered, r)
	}
	if opts.Tail > 0 && len(filtered) > opts.Tail {
		filtered = filtered[len(filtered)-opts.Tail:]
	}

	if p.Format == OutputJSON {
		p.JSON(filtered)
		return nil
	}

	p.Header(fmt.Sprintf("Schedule tick audit — %s", auditPath))
	if len(filtered) == 0 {
		p.Line("  (no tick records match)")
		return nil
	}
	rows := make([][]string, 0, len(filtered))
	for _, r := range filtered {
		ref := r.RunID
		if r.BlockingRunID != "" {
			ref = "⛔ " + r.BlockingRunID
		}
		detail := r.Reason
		if detail == "" {
			detail = r.Error
		}
		rows = append(rows, []string{
			r.At.Local().Format("2006-01-02 15:04:05"),
			r.ScheduleID,
			string(r.Decision),
			ref,
			detail,
		})
	}
	p.Table([]string{"AT", "SCHEDULE", "DECISION", "RUN", "DETAIL"}, rows)
	return nil
}
