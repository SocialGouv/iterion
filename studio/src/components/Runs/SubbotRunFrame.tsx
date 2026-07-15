import type { Node, NodeProps } from "@xyflow/react";
import { Handle, Position } from "@xyflow/react";

import type { RunSummary } from "@/api/runs";
import { NodeIcon } from "@/components/icons/NodeIcon";
import { softColor } from "@/lib/constants";
import { firstOpenChild } from "@/lib/subRuns";

import { statusDotClass } from "./SubRunTabs";

// Container node framing an expanded subbot's child PIPELINE on the RUN
// canvas — the runtime twin of the editor's SubbotFrameNode. A real
// React Flow parent (child nodes carry parentId here; ELK sizes it as a
// compound), solid violet border so it reads as "another bot's graph,
// executing as separate child runs". The header shows one live status
// dot per child run and jumps to the sub-run tabs on click.

const SUBBOT_COLOR = "var(--color-node-subbot)";

export interface SubbotRunFrameData extends Record<string, unknown> {
  label: string;
  source: string;
  isolated: boolean;
  subRuns?: {
    children: RunSummary[];
    onOpen?: (childRunId: string) => void;
  };
}

type SubbotRunFrameType = Node<SubbotRunFrameData, "subbotFrame">;

export default function SubbotRunFrame({ data }: NodeProps<SubbotRunFrameType>) {
  const { label, source, isolated, subRuns } = data;
  const children = subRuns?.children ?? [];
  const openFirst = () => {
    const target = firstOpenChild(children);
    if (target) subRuns?.onOpen?.(target.id);
  };

  return (
    <div
      className="rounded-xl border-2 w-full h-full"
      style={{
        borderColor: softColor(SUBBOT_COLOR, 55),
        borderStyle: "solid",
        background: softColor(SUBBOT_COLOR, 5),
        minWidth: "100%",
        minHeight: "100%",
      }}
    >
      {/* Fallback attachment points for the rare parent edge that stays
          on the frame (child workflow without a done terminal). */}
      <Handle type="target" position={Position.Top} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
      <div
        className="flex items-center gap-1.5 px-3 py-2 select-none"
        style={{ borderBottom: `1px solid ${softColor(SUBBOT_COLOR, 25)}` }}
        title={`Subbot ${label}: runs ${source} as ${children.length || "separate"} child run(s). Click to open the sub-run view.`}
      >
        <NodeIcon kind="subbot" size={14} />
        <span className="text-xs font-semibold text-fg-default">{label}</span>
        <span className="text-[9px] uppercase tracking-wider text-fg-subtle">· subbot</span>
        <span className="font-mono text-caption text-fg-muted truncate">{source}</span>
        {isolated && (
          <span
            className="text-[9px] px-1 rounded"
            style={{ background: softColor(SUBBOT_COLOR, 18), color: SUBBOT_COLOR }}
            title="isolated: each child run confines its writes to its own run store"
          >
            isolated
          </span>
        )}
        {children.length > 0 && (
          <button
            type="button"
            className="ml-auto shrink-0 flex items-center gap-1 px-1.5 py-0.5 rounded border border-border-default bg-surface-1 text-caption text-fg-muted hover:bg-surface-2 hover:text-fg-default transition-colors"
            title={`${children.length} sub-run(s) — open the sub-run view`}
            aria-label={`Open the sub-run view (${children.length} sub-runs)`}
            onClick={(e) => {
              e.stopPropagation();
              openFirst();
            }}
          >
            {children.map((c) => (
              <span
                key={c.id}
                className={`inline-block h-1.5 w-1.5 rounded-full ${statusDotClass(c.status)}`}
                aria-hidden
              />
            ))}
            <span>×{children.length}</span>
          </button>
        )}
      </div>
      <Handle type="source" position={Position.Bottom} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
    </div>
  );
}
