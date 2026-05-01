import type { NodeProps } from "@xyflow/react";
import { Handle, Position } from "@xyflow/react";

import type { ExecStatus } from "@/api/runs";
import type { NodeKind } from "@/api/types";
import { NODE_ICONS } from "@/lib/constants";

// Aggregated status across executions of one IR node — derived in the
// parent canvas. Order of priority: running > failed > paused > finished
// > skipped > none. Drives the border tint so the user can see at a
// glance which IR nodes were "hot" in this run.
export type AggregateStatus = ExecStatus | "none" | "mixed";

const AGG_CLASS: Record<AggregateStatus, string> = {
  running: "border-info bg-info-soft animate-pulse",
  failed: "border-danger bg-danger-soft",
  paused_waiting_human: "border-warning bg-warning-soft",
  finished: "border-success/60 bg-success-soft",
  skipped: "border-border-default bg-surface-2 text-fg-subtle",
  mixed: "border-warning/40 bg-warning-soft/40",
  none: "border-border-default bg-surface-1 text-fg-subtle",
};

interface IRNodeData {
  id: string;
  kind: string;
  count: number;
  status: AggregateStatus;
  isEntry: boolean;
  selected: boolean;
}

export default function IRNode({ data }: NodeProps) {
  const { id, kind, count, status, isEntry, selected } = data as unknown as IRNodeData;
  const klass = AGG_CLASS[status] ?? AGG_CLASS.none;
  const glyph = NODE_ICONS[kind as NodeKind] ?? "";

  return (
    <div
      className={`relative rounded-md border px-3 py-2 shadow-sm w-[180px] text-xs ${klass} ${
        selected ? "ring-2 ring-accent" : ""
      }`}
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
          {count > 0 ? `×${count}` : "—"}
        </span>
      </div>
      <Handle type="source" position={Position.Bottom} className="!bg-fg-subtle" />
    </div>
  );
}
