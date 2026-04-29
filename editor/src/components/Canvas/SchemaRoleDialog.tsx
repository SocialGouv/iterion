import { useEffect, useRef } from "react";

interface SchemaRoleDialogProps {
  x: number;
  y: number;
  onSelect: (role: "input" | "output") => void;
  onClose: () => void;
}

export default function SchemaRoleDialog({ x, y, onSelect, onClose }: SchemaRoleDialogProps) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    window.addEventListener("mousedown", handler);
    return () => window.removeEventListener("mousedown", handler);
  }, [onClose]);

  return (
    <div
      ref={ref}
      className="fixed z-50 bg-surface-1 border border-border-strong rounded-lg shadow-xl p-1 flex flex-col gap-0.5"
      style={{ left: x, top: y }}
    >
      <span className="text-[9px] text-fg-subtle uppercase tracking-wider px-2 pt-1">Assign as</span>
      <button
        className="text-xs text-left px-3 py-1.5 rounded hover:bg-info/20 text-info-fg transition-colors"
        onClick={() => onSelect("input")}
      >
        Input Schema
      </button>
      <button
        className="text-xs text-left px-3 py-1.5 rounded hover:bg-info/20 text-info-fg transition-colors"
        onClick={() => onSelect("output")}
      >
        Output Schema
      </button>
    </div>
  );
}
