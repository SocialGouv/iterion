import { useEffect, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Tooltip } from "@/components/ui";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
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
        <Table caption="Failed dispatches queued for retry" density="sm" className="min-w-full">
          <THead>
            <tr>
              <Th className="whitespace-nowrap">Issue</Th>
              <Th>Attempt</Th>
              <Th className="whitespace-nowrap">Due</Th>
              <Th>Last error</Th>
            </tr>
          </THead>
          <TBody>
            {rows!.map((r) => {
              const dueLabel = formatRetryDue(r.due_at, now);
              const isDue = dueLabel === "due";
              return (
              <Tr
                key={r.issue_id}
                className={`cursor-pointer focus-visible:bg-surface-2/60 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
                  isDue ? "bg-warning-soft" : ""
                }`}
                {...clickableRowProps(() => onFocusIssue(r.issue_id), `Open issue ${r.identifier || r.issue_id} on the board`)}
              >
                <Td className="font-mono whitespace-nowrap">{r.identifier || r.issue_id}</Td>
                <Td>{r.attempt}</Td>
                <Td className="whitespace-nowrap">
                  <span className={isDue ? "text-warning-fg" : "text-fg-muted"}>
                    {dueLabel || relTime(r.due_at)}
                  </span>
                </Td>
                <Td className="text-danger-fg/80 truncate max-w-[24rem]">
                  {r.error}
                </Td>
              </Tr>
              );
            })}
          </TBody>
        </Table>
      )}
    </section>
  );
}
