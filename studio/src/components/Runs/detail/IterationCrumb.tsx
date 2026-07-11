import { useState, type ReactNode } from "react";

import type { ExecutionState } from "@/api/runs";
import { Popover, StatusBadge } from "@/components/ui";
import { formatDurationBetween } from "@/lib/format";

// IterationCrumb renders the "iter: N" position in the breadcrumb
// row. When the node has only one execution it stays a static label;
// for multi-iteration nodes it becomes a button that opens a popover
// listing every attempt with status + duration so the user can jump
// between iterations without leaving the right pane.
export function IterationCrumb({
  exec,
  executions,
  selectedIteration,
  onSelect,
}: {
  exec: ExecutionState;
  executions: ExecutionState[];
  selectedIteration: number;
  onSelect: (iter: number) => void;
}): ReactNode {
  const idx = executions.findIndex((e) => e.execution_id === exec.execution_id);
  const position = idx >= 0 ? idx + 1 : 1;
  const total = executions.length;
  // Primary number is the SEMANTIC loop iteration (loop_iteration =
  // review_loop.iteration), which is exactly what the runtime stamps on
  // each execution and prints in its `node#N` log label. The physical
  // execution index (position) drifts ABOVE it because a resume RE-RUNS
  // a mid-loop iteration, so the same loop_iteration can appear twice in
  // `executions`. Showing loop_iteration makes the crumb next to a
  // reviewer that logged `#48` read 48, not 49. For a plain single non-
  // loop execution (loop_iteration 0, one record) we keep the terse
  // 1-based position so common nodes still read "iter: 1", not "iter: 0".
  const isLooped = total > 1 || exec.loop_iteration > 0;
  const iterLabel = isLooped ? exec.loop_iteration : position;
  // Declared before the single-execution early return so the hook is
  // unconditional (rules-of-hooks); unused when total <= 1.
  const [open, setOpen] = useState(false);
  if (total <= 1) {
    return <span title={`execution ${position} of ${total}`}>iter: {iterLabel}</span>;
  }
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      side="bottom"
      align="start"
      contentClassName="min-w-[220px] p-1.5 text-micro"
      trigger={
        <button
          type="button"
          aria-expanded={open}
          className="hover:text-fg-default underline-offset-2 hover:underline"
          title={`Jump to a different iteration · execution ${position} of ${total}`}
        >
          iter: {iterLabel}
        </button>
      }
    >
      <ul className="space-y-0.5">
        {executions.map((e, i) => {
          const active = i === selectedIteration;
          const duration = formatDurationBetween(e.started_at, e.finished_at);
          return (
            <li key={e.execution_id}>
              <button
                type="button"
                onClick={() => {
                  onSelect(i);
                  setOpen(false);
                }}
                className={`w-full text-left px-2 py-1 rounded flex items-center gap-2 ${
                  active
                    ? "bg-accent-soft text-fg-default"
                    : "hover:bg-surface-2 text-fg-muted"
                }`}
              >
                <span className="font-mono w-6 shrink-0">#{i + 1}</span>
                <span className="text-fg-subtle shrink-0">
                  iter {e.loop_iteration}
                </span>
                <StatusBadge status={e.status} />
                {duration && (
                  <span className="text-fg-subtle ml-auto">{duration}</span>
                )}
              </button>
            </li>
          );
        })}
      </ul>
    </Popover>
  );
}
