import type { DragEvent } from "react";
import type { NodeKind } from "@/api/types";
import { NODE_ICONS, NODE_COLORS } from "@/lib/constants";

const NODE_TYPES: { kind: NodeKind; label: string; desc: string }[] = [
  { kind: "agent", label: "Agent", desc: "LLM-powered agent with model, prompts, tools, and schemas" },
  { kind: "judge", label: "Judge", desc: "Evaluator agent that assesses outputs and makes decisions" },
  { kind: "router", label: "Router", desc: "Splits flow to multiple branches (fan_out_all, condition, round_robin, or llm)" },
  { kind: "human", label: "Human", desc: "Human-in-the-loop step for manual input or review" },
  { kind: "tool", label: "Tool", desc: "Executes a shell command and captures structured output" },
];

export default function NodePalette() {
  const onDragStart = (e: DragEvent, kind: NodeKind) => {
    e.dataTransfer.setData("application/iterion-node", kind);
    e.dataTransfer.effectAllowed = "move";
  };

  return (
    <div className="flex flex-col items-center gap-2 py-3 px-1">
      <span className="text-[9px] text-gray-500 uppercase tracking-wider">Nodes</span>
      {NODE_TYPES.map(({ kind, label, desc }) => (
        <div
          key={kind}
          draggable
          onDragStart={(e) => onDragStart(e, kind)}
          className="w-12 h-12 flex flex-col items-center justify-center rounded cursor-grab hover:brightness-125 transition-all border border-gray-600"
          style={{ backgroundColor: NODE_COLORS[kind] + "33", borderColor: NODE_COLORS[kind] }}
          title={desc}
        >
          <span className="text-base">{NODE_ICONS[kind]}</span>
          <span className="text-[9px] text-gray-300">{label}</span>
        </div>
      ))}
    </div>
  );
}
