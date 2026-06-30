import { useEffect, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Tooltip } from "@/components/ui";
import { clickableRowProps } from "@/lib/a11y";
import type { DispatcherSnapshot } from "@/api/dispatcher";

import { relTime } from "./format";

// useTick re-renders the caller at intervalMs while `active`. Used by
// RetriesTable to keep countdowns smooth without a full dispatcher poll
// each second — the retry table only needs to recompute due_at minus
// now() on its own clock.
function useTick(intervalMs: number, active: boolean): number {
  const [tick, setTick] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setTick(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs, active]);
  return tick;
}

// formatRetryDue returns a short human label for "in 12s" / "due now"
// derived purely from due_at + now. Lives next to RetriesTable so the
// formatting stays scoped to the retry context (the rest of the page
// uses relTime).
function formatRetryDue(dueIso: string, nowMs: number): string {
  if (!dueIso) return "";
  const due = Date.parse(dueIso);
  if (!Number.isFinite(due)) return "";
  const deltaS = Math.round((due - nowMs) / 1000);
  if (deltaS <= 0) return "due";
  if (deltaS < 60) return `in ${deltaS}s`;
  if (deltaS < 3600) return `in ${Math.round(deltaS / 60)}m`;
  return `in ${Math.round(deltaS / 3600)}h`;
}

export interface RetriesTableProps {
  rows: DispatcherSnapshot["retries"];
  canPollDispatches: boolean;
  pollTitle: string;
  onFocusIssue: (issueID: string) => void;
  onRefreshNow: () => void;
}

export default function RetriesTable({
  rows,
  canPollDispatches,
  pollTitle,
  onFocusIssue,
  onRefreshNow,
}: RetriesTableProps) {
  // Tick every 1s when at least one retry is due in under 5 minutes so
  // the countdown is responsive without burning CPU on long-deferred
  // queues.
  const needsTick = (rows ?? []).some((r) => {
    const due = Date.parse(r.due_at);
    return Number.isFinite(due) && due - Date.now() < 5 * 60_000;
  });
  const now = useTick(1000, needsTick);
  return (
    <section className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)] overflow-hidden">
      <header className="px-4 py-2 border-b border-border-default text-title font-semibold flex items-center justify-between gap-2">
        <span>Retry queue ({rows?.length ?? 0})</span>
        {rows && rows.length > 0 && (
          <Tooltip content={pollTitle}>
            <Button
              variant="secondary"
              size="sm"
              onClick={onRefreshNow}
              disabled={!canPollDispatches}
            >
              Poll now
            </Button>
          </Tooltip>
        )}
      </header>
      {!rows || rows.length === 0 ? (
        <div className="p-4 text-xs text-fg-muted">No retries pending.</div>
      ) : (
        <div className="overflow-x-auto">
        <table className="min-w-full text-xs">
          <thead className="text-fg-muted border-b border-border-default">
            <tr>
              <th className="text-left py-1.5 px-3 font-normal whitespace-nowrap">Issue</th>
              <th className="text-left py-1.5 px-3 font-normal">Attempt</th>
              <th className="text-left py-1.5 px-3 font-normal whitespace-nowrap">Due</th>
              <th className="text-left py-1.5 px-3 font-normal">Last error</th>
            </tr>
          </thead>
          <tbody>
            {rows!.map((r) => {
              const dueLabel = formatRetryDue(r.due_at, now);
              const isDue = dueLabel === "due";
              return (
              <tr
                key={r.issue_id}
                className={`border-b border-border-default/60 hover:bg-surface-2/40 cursor-pointer focus-visible:bg-surface-2/60 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
                  isDue ? "bg-warning-soft" : ""
                }`}
                {...clickableRowProps(() => onFocusIssue(r.issue_id), `Open issue ${r.identifier || r.issue_id} on the board`)}
              >
                <td className="py-1.5 px-3 font-mono whitespace-nowrap">{r.identifier || r.issue_id}</td>
                <td className="py-1.5 px-3">{r.attempt}</td>
                <td className="py-1.5 px-3 whitespace-nowrap">
                  <span className={isDue ? "text-warning-fg" : "text-fg-muted"}>
                    {dueLabel || relTime(r.due_at)}
                  </span>
                </td>
                <td className="py-1.5 px-3 text-danger-fg/80 truncate max-w-[24rem]">
                  {r.error}
                </td>
              </tr>
              );
            })}
          </tbody>
        </table>
        </div>
      )}
    </section>
  );
}
