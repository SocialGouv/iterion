import type { ReactNode } from "react";
import { Link } from "wouter";

import { formatDateTime } from "@/lib/format";
import { runErrorCode, runErrorHint } from "@/lib/runErrorHints";

// RunOutcomeRecord is the tuple every record that LAUNCHES runs now carries
// (#1501): a cloud schedule, a trigger subscription, a board card. The
// run-outcome spine stamps it from the run's terminal status, so a record
// that fired and whose run died stops reading like one that fired and worked.
//
// last_error is the launch-time verdict that PRECEDES the run — a different
// record of a different moment (an org launch-gate denial, a run service that
// refused). Both are rendered: a later refused tick must not hide the
// previous run's outcome, and a successful run must not hide a refusal since.
export interface RunOutcomeRecord {
  last_run_id?: string;
  last_run_status?: string;
  last_run_error?: string;
  last_run_error_code?: string;
  last_run_at?: string;
  last_error?: string;
}

// runOutcomeFailed answers "did the last run go wrong" from the STATUS, not
// from the presence of a message: the terminal statuses are finished, failed,
// failed_resumable and cancelled (store.RunStatus), and a cancelled run
// carries no error text at all — keyed on the message alone it would read as
// a success. Records written before the status existed are still judged by
// their message, so an older failure keeps its tone.
export function runOutcomeFailed(record: RunOutcomeRecord): boolean {
  const status = record.last_run_status?.trim();
  if (status) return status !== "finished";
  return !!(record.last_run_error_code?.trim() || record.last_run_error?.trim());
}

interface Props {
  record: RunOutcomeRecord;
  /** What the operator should do about a failure on THIS record kind (the
   *  schedule keeps its cadence, the trigger stays armed…). Rendered only
   *  when there is a failure or a refusal to advise about. */
  trailer?: ReactNode;
}

/**
 * LastRunOutcome renders one record's run outcome: the terminal status and
 * when it was stamped, a link to the run, the stable failure code, the raw
 * message and the code's remedy hint — then the launch refusal, which is a
 * separate fact. Nothing is rendered before the first outcome: an empty row
 * would read as a verdict.
 *
 * One component for the whole class (schedule row, trigger row, board card)
 * so the same tuple cannot say "Last run: failed" on one surface and nothing
 * on another; a fourth record kind renders it, it does not re-implement it.
 */
export default function LastRunOutcome({ record, trailer }: Props) {
  const code = runErrorCode({ failure_code: record.last_run_error_code, error: record.last_run_error });
  const failed = runOutcomeFailed(record);
  // The hint answers a CAUSE: a cancelled run (no code, no message) is a
  // failure to show, not something to advise about.
  const hasCause = !!(code || record.last_run_error);
  const hint = hasCause ? runErrorHint(code, { error: record.last_run_error }) : null;
  if (!record.last_run_status && !failed && !record.last_error) return null;

  return (
    <div className="w-full min-w-0 space-y-1 text-caption">
      {(record.last_run_status || failed) && (
        <div className={failed ? "text-warning-fg" : "text-fg-muted"}>
          <span>Last run: {record.last_run_status || "failed"}</span>
          {record.last_run_at && <span> · {formatDateTime(record.last_run_at)}</span>}
          {record.last_run_id && (
            <Link
              className="ml-2 text-accent-text underline underline-offset-2 hover:decoration-2"
              href={`/runs/${encodeURIComponent(record.last_run_id)}`}
            >
              View run
            </Link>
          )}
          {code && <span className="ml-2 font-mono">{code}</span>}
          {record.last_run_error && <p className="whitespace-pre-wrap break-words">{record.last_run_error}</p>}
          {hint && <p>{hint}</p>}
        </div>
      )}
      {record.last_error && (
        <p className="text-warning-fg whitespace-pre-wrap break-words">Last launch refused: {record.last_error}</p>
      )}
      {(failed || record.last_error) && trailer && <p className="text-fg-muted">{trailer}</p>}
    </div>
  );
}
