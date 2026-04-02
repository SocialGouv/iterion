import type { DragEvent } from "react";
import type { NodeKind } from "@/api/types";

const NODE_TYPES: { kind: NodeKind; icon: string; label: string; desc: string }[] = [
  { kind: "agent", icon: "\u{1F916}", label: "Agent", desc: "LLM-powered agent with model, prompts, tools, and schemas" },
  { kind: "judge", icon: "\u{2696}\u{FE0F}", label: "Judge", desc: "Evaluator agent that assesses outputs and makes decisions" },
  { kind: "router", icon: "\u{1F504}", label: "Router", desc: "Splits flow to multiple branches (fan_out_all, condition, round_robin, or llm)" },
  { kind: "join", icon: "\u{1F91D}", label: "Join", desc: "Merges parallel branches back together (wait_all or best_effort)" },
  { kind: "human", icon: "\u{1F464}", label: "Human", desc: "Human-in-the-loop step for manual input or review" },
  { kind: "tool", icon: "\u{1F527}", label: "Tool", desc: "Executes a shell command and captures structured output" },
];

const COLORS: Record<string, string> = {
  agent: "#4A90D9",
  judge: "#7B68EE",
  router: "#E67E22",
  join: "#2ECC71",
  human: "#E74C3C",
  tool: "#8B6914",
};

export default function NodePalette() {
  const onDragStart = (e: DragEvent, kind: NodeKind) => {
    e.dataTransfer.setData("application/iterion-node", kind);
    e.dataTransfer.effectAllowed = "move";
  };

  return (
    <div className="flex flex-col items-center gap-2 py-3 px-1">
      <span className="text-[9px] text-gray-500 uppercase tracking-wider">Nodes</span>
      {NODE_TYPES.map(({ kind, icon, label, desc }) => (
        <div
          key={kind}
          draggable
          onDragStart={(e) => onDragStart(e, kind)}
          className="w-12 h-12 flex flex-col items-center justify-center rounded cursor-grab hover:brightness-125 transition-all border border-gray-600"
          style={{ backgroundColor: COLORS[kind] + "33", borderColor: COLORS[kind] }}
          title={desc}
        >
          <span className="text-base">{icon}</span>
          <span className="text-[9px] text-gray-300">{label}</span>
        </div>
      ))}
    </div>
  );
}
