import { useEffect, useRef } from "react";
import type { NodeKind } from "@/api/types";
import { NODE_ICONS } from "@/lib/constants";

const QUICK_ADD_TYPES: { kind: NodeKind; label: string }[] = [
  { kind: "agent", label: "Agent" },
  { kind: "judge", label: "Judge" },
  { kind: "router", label: "Router" },
  { kind: "join", label: "Join" },
  { kind: "human", label: "Human" },
  { kind: "tool", label: "Tool" },
];

interface Props {
  x: number;
  y: number;
  sourceId: string;
  onAddNode: (kind: NodeKind) => void;
  onConnectTerminal: (target: "done" | "fail") => void;
  onClose: () => void;
}

export default function QuickAddMenu({ x, y, onAddNode, onConnectTerminal, onClose }: Props) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        onClose();
      }
    };
    const keyHandler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.document.addEventListener("mousedown", handler);
    window.addEventListener("keydown", keyHandler);
    return () => {
      window.document.removeEventListener("mousedown", handler);
      window.removeEventListener("keydown", keyHandler);
    };
  }, [onClose]);

  return (
    <div
      ref={ref}
      className="fixed bg-gray-800 border border-gray-600 rounded-lg shadow-xl z-50 py-1 min-w-[140px]"
      style={{
        left: Math.min(x, window.innerWidth - 160),
        top: Math.min(y, window.innerHeight - 340),
      }}
    >
      <div className="px-3 py-1 text-[10px] text-gray-500 uppercase tracking-wider">Add node</div>
      {QUICK_ADD_TYPES.map(({ kind, label }) => (
        <button
          key={kind}
          className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
          onClick={() => onAddNode(kind)}
        >
          <span>{NODE_ICONS[kind]}</span>
          {label}
        </button>
      ))}
      <div className="border-t border-gray-700 my-1" />
      <div className="px-3 py-1 text-[10px] text-gray-500 uppercase tracking-wider">Connect to</div>
      <button
        className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
        onClick={() => onConnectTerminal("done")}
      >
        <span>{"\u{2705}"}</span>
        done
      </button>
      <button
        className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
        onClick={() => onConnectTerminal("fail")}
      >
        <span>{"\u{274C}"}</span>
        fail
      </button>
      <div className="border-t border-gray-700 my-1" />
      <button
        className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-gray-400 flex items-center gap-2"
        onClick={onClose}
      >
        Cancel
      </button>
    </div>
  );
}
