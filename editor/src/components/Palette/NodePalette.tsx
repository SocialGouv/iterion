import type { DragEvent } from "react";
import type { NodeKind } from "@/api/types";

const NODE_TYPES: { kind: NodeKind; icon: string; label: string }[] = [
  { kind: "agent", icon: "\u{1F916}", label: "Agent" },
  { kind: "judge", icon: "\u{2696}\u{FE0F}", label: "Judge" },
  { kind: "router", icon: "\u{1F504}", label: "Router" },
  { kind: "join", icon: "\u{1F91D}", label: "Join" },
  { kind: "human", icon: "\u{1F464}", label: "Human" },
  { kind: "tool", icon: "\u{1F527}", label: "Tool" },
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
      {NODE_TYPES.map(({ kind, icon, label }) => (
        <div
          key={kind}
          draggable
          onDragStart={(e) => onDragStart(e, kind)}
          className="w-12 h-12 flex flex-col items-center justify-center rounded cursor-grab hover:brightness-125 transition-all border border-gray-600"
          style={{ backgroundColor: COLORS[kind] + "33", borderColor: COLORS[kind] }}
          title={label}
        >
          <span className="text-base">{icon}</span>
          <span className="text-[9px] text-gray-300">{label}</span>
        </div>
      ))}
    </div>
  );
}
