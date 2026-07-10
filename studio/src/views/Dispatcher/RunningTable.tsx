import { Button } from "@/components/ui/Button";
import { Tooltip } from "@/components/ui";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
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
        // Table's built-in overflow-x-auto keeps the 7-column row fully
        // reachable when the page is capped by max-w-4xl on small viewports.
        <Table caption="Runs the dispatcher currently has in flight" density="sm" className="min-w-full">
          <THead>
            <tr>
              <Th className="whitespace-nowrap">Identifier</Th>
              <Th>Run</Th>
              <Th>State</Th>
              <Th>Workspace</Th>
              <Th className="whitespace-nowrap">Started</Th>
              <Th className="whitespace-nowrap">Last event</Th>
              <Th align="right">Actions</Th>
            </tr>
          </THead>
          <TBody>
            {rows!.map((r) => {
              const stall = stallStyle(r.last_event_at, stallTimeoutS);
              return (
              <Tr
                key={r.issue_id}
                // Disable the hover tint on stalled rows so the amber/red
                // status background stays visible under the cursor.
                hover={!stall.rowClass}
                className={stall.rowClass}
                title={stall.hint ?? undefined}
              >
                <Td className="font-mono whitespace-nowrap">{r.identifier}</Td>
                <Td className="font-mono truncate max-w-[14rem]">
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
                </Td>
                <Td>{r.workflow_state}</Td>
                <Td
                  className="font-mono text-fg-muted truncate max-w-[18rem]"
                  title={r.workspace_path ?? "no workspace path captured (legacy or in-process run)"}
                >
                  {r.workspace_path ? compactWorkspace(r.workspace_path) : <span className="text-fg-subtle">—</span>}
                </Td>
                <Td className="text-fg-muted whitespace-nowrap">{relTime(r.started_at)}</Td>
                <Td className="text-fg-muted whitespace-nowrap">
                  {r.last_event_name ? r.last_event_name + " · " : ""}
                  {relTime(r.last_event_at)}
                  {stall.hint && (
                    <span className="ml-1 text-warning-fg/90">⏱</span>
                  )}
                </Td>
                <Td align="right">
                  <Button
                    variant="danger"
                    size="sm"
                    onClick={() => onCancel(r.issue_id)}
                  >
                    Cancel
                  </Button>
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
