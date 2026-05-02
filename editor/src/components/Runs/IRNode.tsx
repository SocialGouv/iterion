import { useState } from "react";
import type { NodeProps } from "@xyflow/react";
import { Handle, Position } from "@xyflow/react";

import type { ExecutionState } from "@/api/runs";
import type { NodeKind } from "@/api/types";
import { Popover } from "@/components/ui";
import { NODE_ICONS } from "@/lib/constants";

import { statusClasses } from "./runStatusClasses";

// Maximum pips to show inline before condensing into a "+N" overflow
// affordance. Tuned to match the 200px node width — beyond ~6 the strip
// either wraps or the node grows taller.
const INLINE_PIPS_MAX = 6;

// Palette cycled through iteration indices so a loop body that fired
// 5 times shows pip 0 cyan, pip 1 violet, pip 2 amber, etc. Independent
// of status colors — the eye can track "which iteration?" separately
// from "did it succeed?".
export const ITERATION_PALETTE = [
  "#06B6D4", // cyan
  "#A855F7", // purple
  "#F59E0B", // amber
  "#14B8A6", // teal
  "#EC4899", // pink
  "#84CC16", // lime
];

export function iterationColor(index: number): string {
  return ITERATION_PALETTE[index % ITERATION_PALETTE.length]!;
}

interface IRNodeData {
  id: string;
  kind: string;
  // All executions of this IR node, sorted by loop_iteration ascending.
  // Empty list = node hasn't been visited yet in this run.
  executions: ExecutionState[];
  // Currently selected iteration index. When >0 executions exist and
  // selectedIteration matches one of them, that exec drives the
  // border + status; otherwise we fall back to the latest.
  selectedIteration: number;
  isEntry: boolean;
  selected: boolean;
  onSelectIteration: (nodeId: string, iteration: number) => void;
}

export default function IRNode({ data }: NodeProps) {
  const { id, kind, executions, selectedIteration, isEntry, selected, onSelectIteration } =
    data as unknown as IRNodeData;
  const glyph = NODE_ICONS[kind as NodeKind] ?? "";

  const activeExec =
    executions.find((e) => e.loop_iteration === selectedIteration) ??
    executions[executions.length - 1] ??
    null;
  const status = activeExec?.status ?? "none";
  const c = statusClasses(status);

  return (
    <div
      className={`relative rounded-md border px-3 py-2 shadow-sm w-[200px] text-xs transition-colors duration-300 ${c.bg} ${c.border} ${c.text} ${
        selected ? "ring-2 ring-accent" : ""
      }`}
      // Inner ring tinted by the selected iteration index so the user
      // can scan the canvas and see "iteration 3 → all the violet
      // borders". Subtle so it doesn't fight the status color.
      style={
        executions.length > 0
          ? { boxShadow: `inset 0 0 0 2px ${iterationColor(selectedIteration)}33` }
          : undefined
      }
    >
      <Handle type="target" position={Position.Top} className="!bg-fg-subtle" />
      <div className="flex items-center gap-1.5 font-medium truncate">
        <span aria-hidden>{glyph}</span>
        <span className="truncate" title={id}>
          {id}
        </span>
        {isEntry && (
          <span
            className="ml-auto text-[9px] uppercase bg-warning-soft text-warning-fg px-1 rounded"
            title="entry node"
          >
            entry
          </span>
        )}
      </div>
      <div className="mt-1 flex items-center gap-1.5 text-[10px] text-fg-subtle">
        <span>{kind}</span>
        <span className="ml-auto">
          {executions.length === 0
            ? "—"
            : executions.length === 1
            ? status
            : `${status} · iter ${selectedIteration + 1}/${executions.length}`}
        </span>
      </div>

      {/* Iteration timeline: one pip per execution. Pip color =
          iteration palette (index-based). Filled when status is
          terminal; pulse when running. Selected pip has thicker
          border + scale. Click to switch. */}
      {executions.length > 1 && (
        <IterationTimeline
          nodeId={id}
          executions={executions}
          selectedIteration={selectedIteration}
          onSelectIteration={onSelectIteration}
        />
      )}

      <Handle type="source" position={Position.Bottom} className="!bg-fg-subtle" />
    </div>
  );
}

function IterationTimeline({
  nodeId,
  executions,
  selectedIteration,
  onSelectIteration,
}: {
  nodeId: string;
  // Already sorted by loop_iteration ascending — RunCanvasIR.execsByNode
  // does the sort once at grouping time so we don't re-sort per render.
  executions: ExecutionState[];
  selectedIteration: number;
  onSelectIteration: (nodeId: string, iteration: number) => void;
}) {
  const overflow = executions.length > INLINE_PIPS_MAX;
  // When overflowing, show INLINE_PIPS_MAX-1 most-recent pips + the
  // "+N" affordance. If the selected iteration falls outside that
  // window, also pin it inline so the user always sees what they
  // currently have selected.
  let inline: ExecutionState[];
  let extra: ExecutionState[];
  if (!overflow) {
    inline = executions;
    extra = [];
  } else {
    const tail = executions.slice(-(INLINE_PIPS_MAX - 1));
    const tailSet = new Set(tail.map((e) => e.execution_id));
    const selectedExec = executions.find(
      (e) => e.loop_iteration === selectedIteration,
    );
    if (selectedExec && !tailSet.has(selectedExec.execution_id)) {
      // Insert the selected pip at the front, dropping the oldest
      // tail entry to keep the strip width bounded.
      inline = [selectedExec, ...tail.slice(1)];
    } else {
      inline = tail;
    }
    extra = executions.filter((e) => !inline.includes(e));
  }

  return (
    <div
      className="mt-1.5 flex items-center gap-1 flex-wrap"
      onClick={(e) => {
        // ReactFlow's node-click handler would otherwise fire when the
        // user clicks inside the pip strip.
        e.stopPropagation();
      }}
    >
      {inline.map((exec) => (
        <IterationPip
          key={exec.execution_id}
          exec={exec}
          selected={exec.loop_iteration === selectedIteration}
          onClick={() => onSelectIteration(nodeId, exec.loop_iteration)}
        />
      ))}
      {extra.length > 0 && (
        <OverflowPopover
          label={`+${extra.length}`}
          executions={extra}
          selectedIteration={selectedIteration}
          onSelectIteration={(it) => onSelectIteration(nodeId, it)}
        />
      )}
    </div>
  );
}

function IterationPip({
  exec,
  selected,
  onClick,
}: {
  exec: ExecutionState;
  selected: boolean;
  onClick: () => void;
}) {
  const iter = exec.loop_iteration;
  const palette = iterationColor(iter);
  const statusGlyph = statusClasses(exec.status).glyph;
  return (
    <button
      type="button"
      onClick={onClick}
      className={`flex items-center justify-center text-[9px] font-mono rounded ${
        selected
          ? "ring-1 ring-fg-default scale-110"
          : "opacity-70 hover:opacity-100"
      }`}
      style={{
        backgroundColor: `${palette}33`,
        border: `1px solid ${palette}`,
        color: palette,
        minWidth: 18,
        height: 16,
        padding: "0 3px",
      }}
      title={`Iteration ${iter + 1} · ${exec.status}`}
    >
      {iter + 1}
      <span className="ml-0.5">{statusGlyph}</span>
    </button>
  );
}

function OverflowPopover({
  label,
  executions,
  selectedIteration,
  onSelectIteration,
}: {
  label: string;
  executions: ExecutionState[];
  selectedIteration: number;
  onSelectIteration: (iteration: number) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      trigger={
        <button
          type="button"
          className="flex items-center justify-center text-[9px] font-mono rounded bg-surface-2 border border-border-default text-fg-muted hover:text-fg-default hover:bg-surface-3"
          style={{
            minWidth: 22,
            height: 16,
            padding: "0 3px",
          }}
          title={`${executions.length} more iterations`}
        >
          {label}
        </button>
      }
    >
      <div className="grid grid-cols-4 gap-1 p-2 max-w-[180px]">
        {executions.map((exec) => (
          <IterationPip
            key={exec.execution_id}
            exec={exec}
            selected={exec.loop_iteration === selectedIteration}
            onClick={() => {
              onSelectIteration(exec.loop_iteration);
              setOpen(false);
            }}
          />
        ))}
      </div>
    </Popover>
  );
}

