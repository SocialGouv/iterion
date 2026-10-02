import { Link } from "wouter";
import type { ScheduledBot } from "@/api/schedules";
import { formatDateTime } from "@/lib/format";
import { runErrorCode, runErrorHint } from "@/lib/runErrorHints";

// Launch refusals and completed-run failures are different records. Show
// both so a later refused tick cannot hide the previous run's outcome.
export default function ScheduleOutcome({ schedule: s }: { schedule: ScheduledBot }) {
  const code = runErrorCode({ failure_code: s.last_run_error_code, error: s.last_run_error });
  const hasFailure = !!(code || s.last_run_error);
  const hint = hasFailure ? runErrorHint(code, { error: s.last_run_error }) : null;
  if (!s.last_run_status && !hasFailure && !s.last_error) return null;

  return (
    <div className="w-full min-w-0 space-y-1 text-caption">
      {(s.last_run_status || hasFailure) && (
        <div className={hasFailure ? "text-warning-fg" : "text-fg-muted"}>
          <span>Last run: {s.last_run_status || "failed"}</span>
          {s.last_run_at && <span> · {formatDateTime(s.last_run_at)}</span>}
          {s.last_run_id && (
            <Link className="ml-2 text-accent-text hover:underline" href={`/runs/${encodeURIComponent(s.last_run_id)}`}>
              View run
            </Link>
          )}
          {code && <span className="ml-2 font-mono">{code}</span>}
          {s.last_run_error && <p className="whitespace-pre-wrap break-words">{s.last_run_error}</p>}
          {hint && <p>{hint}</p>}
        </div>
      )}
      {s.last_error && (
        <p className="text-warning-fg whitespace-pre-wrap break-words">
          Last launch refused: {s.last_error}
        </p>
      )}
      {(hasFailure || s.last_error) && (
        <p className="text-fg-muted">
          {s.disabled ? "Schedule paused." : "The schedule keeps its cadence. Pause it while fixing the cause if needed."}
        </p>
      )}
    </div>
  );
}
