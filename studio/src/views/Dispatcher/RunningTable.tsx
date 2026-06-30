import { Button } from "@/components/ui/Button";
import { Tooltip } from "@/components/ui";
import type { DispatcherSnapshot } from "@/api/dispatcher";

import { compactWorkspace, relTime } from "./format";

// stallStyle inspects how long a running entry has been silent relative
// to the dispatcher's stallTimeout and returns a (className, hint) pair
// for the row. The thresholds match what the operator can act on:
//   ≥ 50% of the budget elapsed → amber  ("slow — keep an eye")
//   ≥ 100% of the budget        → red    ("about to be cancelled")
// Below 50%: no decoration, the row reads as normal.
function stallStyle(
  lastEventAt: string,
  stallTimeoutS: number,
): { rowClass: string; hint: string | null } {
  if (!stallTimeoutS || stallTimeoutS <= 0) {
    return { rowClass: "", hint: null };
  }
  const last = Date.parse(lastEventAt);
  if (!Number.isFinite(last)) return { rowClass: "", hint: null };
  const elapsedS = (Date.now() - last) / 1000;
  if (elapsedS >= stallTimeoutS) {
    return {
      rowClass: "bg-danger-soft",
      hint: `Silent for ${Math.round(elapsedS)}s ≥ stall timeout (${Math.round(stallTimeoutS)}s) — will be cancelled on the next reconciliation tick.`,
    };
  }
  if (elapsedS >= stallTimeoutS / 2) {
    return {
      rowClass: "bg-warning-soft",
      hint: `Silent for ${Math.round(elapsedS)}s — half the stall budget (${Math.round(stallTimeoutS)}s) consumed.`,
    };
  }
  return { rowClass: "", hint: null };
}

export interface RunningTableProps {
  rows: DispatcherSnapshot["running"];
  stallTimeoutS: number;
  onCancel: (id: string) => void;
  onOpenRun: (runID: string) => void;
}

export default function RunningTable({
  rows,
  stallTimeoutS,
  onCancel,
  onOpenRun,
}: RunningTableProps) {
  return (
    <section className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)] overflow-hidden">
      <header className="px-4 py-2 border-b border-border-default text-title font-semibold">
        Running ({rows?.length ?? 0})
      </header>
      {!rows || rows.length === 0 ? (
        <div className="p-4 text-xs text-fg-muted">No runs in flight.</div>
      ) : (
        // overflow-x-auto so the 7-column row stays fully reachable
        // when the page is capped by max-w-4xl on small viewports.
        <div className="overflow-x-auto">
        <table className="min-w-full text-xs">
          <thead className="text-fg-muted border-b border-border-default">
            <tr>
              <th className="text-left py-1.5 px-3 font-normal whitespace-nowrap">Identifier</th>
              <th className="text-left py-1.5 px-3 font-normal">Run</th>
              <th className="text-left py-1.5 px-3 font-normal">State</th>
              <th className="text-left py-1.5 px-3 font-normal">Workspace</th>
              <th className="text-left py-1.5 px-3 font-normal whitespace-nowrap">Started</th>
              <th className="text-left py-1.5 px-3 font-normal whitespace-nowrap">Last event</th>
              <th className="text-right py-1.5 px-3 font-normal">Actions</th>
            </tr>
          </thead>
          <tbody>
            {rows!.map((r) => {
              const stall = stallStyle(r.last_event_at, stallTimeoutS);
              return (
              <tr
                key={r.issue_id}
                className={`border-b border-border-default/60 ${stall.rowClass}`}
                title={stall.hint ?? undefined}
              >
                <td className="py-1.5 px-3 font-mono whitespace-nowrap">{r.identifier}</td>
                <td className="py-1.5 px-3 font-mono truncate max-w-[14rem]">
                  <button
                    type="button"
                    onClick={() => onOpenRun(r.run_id)}
                    className="text-info hover:underline"
                    title={`Open run ${r.run_id}`}
                  >
                    {r.run_id}
                  </button>
                  {r.attempt && r.attempt > 0 ? (
                    <Tooltip
                      content={`Resume of a prior failed_resumable run — attempt ${r.attempt + 1}. The dispatcher continues from the failing node's checkpoint instead of starting fresh.`}
                    >
                      <span className="ml-1.5 inline-flex items-center rounded bg-warning-soft text-warning-fg px-1.5 py-0.5 text-caption font-mono align-middle">
                        resume #{r.attempt + 1}
                      </span>
                    </Tooltip>
                  ) : null}
                </td>
                <td className="py-1.5 px-3">{r.workflow_state}</td>
                <td
                  className="py-1.5 px-3 font-mono text-fg-muted truncate max-w-[18rem]"
                  title={r.workspace_path ?? "no workspace path captured (legacy or in-process run)"}
                >
                  {r.workspace_path ? compactWorkspace(r.workspace_path) : <span className="text-fg-subtle">—</span>}
                </td>
                <td className="py-1.5 px-3 text-fg-muted whitespace-nowrap">{relTime(r.started_at)}</td>
                <td className="py-1.5 px-3 text-fg-muted whitespace-nowrap">
                  {r.last_event_name ? r.last_event_name + " · " : ""}
                  {relTime(r.last_event_at)}
                  {stall.hint && (
                    <span className="ml-1 text-warning-fg/90">⏱</span>
                  )}
                </td>
                <td className="py-1.5 px-3 text-right">
                  <Button
                    variant="danger"
                    size="sm"
                    onClick={() => onCancel(r.issue_id)}
                  >
                    Cancel
                  </Button>
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
