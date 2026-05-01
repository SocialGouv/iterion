import type { NodeProps } from "@xyflow/react";
import { Handle, Position } from "@xyflow/react";

import type { ExecutionState } from "@/api/runs";
import { NODE_ICONS } from "@/lib/constants";
import type { NodeKind } from "@/api/types";

const STATUS_CLASS: Record<ExecutionState["status"], string> = {
  running:
    "bg-info-soft border-info text-fg-default animate-pulse",
  finished:
    "bg-success-soft border-success/60 text-fg-default",
  failed:
    "bg-danger-soft border-danger/60 text-fg-default",
  paused_waiting_human:
    "bg-warning-soft border-warning/60 text-fg-default",
  skipped: "bg-surface-2 border-border-default text-fg-subtle",
};

interface NodeData {
  exec: ExecutionState;
  selected: boolean;
}

export default function ExecutionNode({ data }: NodeProps) {
  const { exec, selected } = data as unknown as NodeData;
  const klass = STATUS_CLASS[exec.status] ?? STATUS_CLASS.skipped;
  const glyph = exec.kind ? NODE_ICONS[exec.kind as NodeKind] ?? "" : "";

  return (
    <div
      className={`relative rounded-md border px-3 py-2 shadow-sm w-[180px] text-xs ${klass} ${
        selected ? "ring-2 ring-accent" : ""
      }`}
    >
      <Handle type="target" position={Position.Top} className="!bg-fg-subtle" />
      <div className="flex items-center gap-1.5 font-medium truncate">
        <span aria-hidden>{glyph}</span>
        <span className="truncate">{exec.ir_node_id}</span>
        {exec.loop_iteration > 0 && (
          <span className="ml-auto text-[10px] text-fg-subtle">#{exec.loop_iteration}</span>
        )}
      </div>
      <div className="mt-1 flex items-center gap-1.5 text-[10px] text-fg-subtle">
        <span className="truncate">{exec.branch_id}</span>
        <span className="ml-auto">{exec.status}</span>
      </div>
      <Handle type="source" position={Position.Bottom} className="!bg-fg-subtle" />
    </div>
  );
}
