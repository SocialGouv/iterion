import type { ExecutionState } from "@/api/runs";
import { softColor } from "@/lib/constants";

import { iterationColor } from "../IRNode";
import { statusClasses } from "../runStatusClasses";

// IterationPills mirrors the per-iteration pip strip already shown on
// the canvas node (IRNode), but in the right-panel header. The colors
// come from the same ITERATION_PALETTE so a user scanning the canvas
// for "iter 3" sees the same amber tint in both places. Status is
// overlaid as a ring/animation so the pill carries two dimensions:
// which iteration (color) and how it went (ring/pulse/saturation).
export function IterationPills({
  executions,
  selectedIteration,
  onSelect,
}: {
  executions: ExecutionState[];
  // 0-based index into `executions` of the currently selected attempt.
  selectedIteration: number;
  // Callback receives the selected attempt's array index.
  onSelect: (index: number) => void;
}) {
  return (
    <div className="mt-1 flex flex-wrap items-center gap-1">
      <span className="text-[9px] text-fg-subtle mr-0.5">iter:</span>
      {executions.map((e, idx) => {
        const isSelected = idx === selectedIteration;
        const s = statusClasses(e.status);
        const color = iterationColor(idx);
        // Selection is rendered as a thicker ring in accent color so
        // the active pill pops; the iteration color stays as the
        // fill. Status drives extra cues:
        //   running → animate-pulse (matches StatusBadge running)
        //   failed  → red ring overlay
        //   skipped → desaturated (engine bypassed this iter)
        const pulse = e.status === "running" ? "animate-pulse" : "";
        const opacity = e.status === "skipped" ? "saturate-0" : "";
        const failedRing = e.status === "failed" ? "ring-1 ring-danger" : "";
        const selectedRing = isSelected
          ? "ring-2 ring-accent shadow-sm"
          : "ring-1 ring-border-default/30";
        return (
          <button
            key={e.execution_id}
            type="button"
            onClick={() => onSelect(idx)}
            title={`iter ${idx + 1} · ${s.label}`}
            className={`inline-flex items-center justify-center min-w-[18px] h-[18px] px-1 rounded-full text-[9px] font-mono text-fg-default transition-all ${pulse} ${opacity} ${selectedRing} ${failedRing}`}
            style={{ backgroundColor: softColor(color, 40) }}
          >
            {idx + 1}
          </button>
        );
      })}
    </div>
  );
}
