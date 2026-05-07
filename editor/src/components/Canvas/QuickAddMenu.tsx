import { useEffect, useRef } from "react";
import type { NodeKind } from "@/api/types";
import { NodeIcon } from "@/components/icons/NodeIcon";

const QUICK_ADD_TYPES: { kind: NodeKind; label: string }[] = [
  { kind: "agent", label: "Agent" },
  { kind: "judge", label: "Judge" },
  { kind: "router", label: "Router" },
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
      className="fixed bg-surface-1 border border-border-strong rounded-lg shadow-xl z-50 py-1 min-w-[140px]"
      style={{
        left: Math.min(x, window.innerWidth - 160),
        top: Math.min(y, window.innerHeight - 340),
      }}
    >
      <div className="px-3 py-1 text-[10px] text-fg-subtle uppercase tracking-wider">Add node</div>
      {QUICK_ADD_TYPES.map(({ kind, label }) => (
        <button
          key={kind}
          className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-default flex items-center gap-2"
          onClick={() => onAddNode(kind)}
        >
          <NodeIcon kind={kind} size={14} />
          {label}
        </button>
      ))}
      <div className="border-t border-border-default my-1" />
      <div className="px-3 py-1 text-[10px] text-fg-subtle uppercase tracking-wider">Connect to</div>
      <button
        className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-default flex items-center gap-2"
        onClick={() => onConnectTerminal("done")}
      >
        <NodeIcon kind="done" size={14} />
        done
      </button>
      <button
        className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-default flex items-center gap-2"
        onClick={() => onConnectTerminal("fail")}
      >
        <NodeIcon kind="fail" size={14} />
        fail
      </button>
      <div className="border-t border-border-default my-1" />
      <button
        className="w-full text-left px-3 py-1.5 hover:bg-surface-2 text-xs text-fg-subtle flex items-center gap-2"
        onClick={onClose}
      >
        Cancel
      </button>
    </div>
  );
}
