import { useEffect, useRef } from "react";

interface Props {
  x: number;
  y: number;
  nodeId: string;
  isTerminal: boolean;
  isEntry: boolean;
  onSetEntry: () => void;
  onDuplicate: () => void;
  onDelete: () => void;
  onClose: () => void;
}

export default function NodeContextMenu({
  x,
  y,
  nodeId,
  isTerminal,
  isEntry,
  onSetEntry,
  onDuplicate,
  onDelete,
  onClose,
}: Props) {
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
      className="fixed bg-gray-800 border border-gray-600 rounded-lg shadow-xl z-50 py-1 min-w-[160px]"
      style={{ left: x, top: y }}
    >
      <div className="px-3 py-1 text-[10px] text-gray-500 uppercase tracking-wider">
        {nodeId}
      </div>
      {!isTerminal && (
        <>
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
            onClick={() => { onSetEntry(); onClose(); }}
            disabled={isEntry}
          >
            <span className="text-amber-400">&#x25B6;</span>
            {isEntry ? "Already entry point" : "Set as entry point"}
          </button>
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs text-white flex items-center gap-2"
            onClick={() => { onDuplicate(); onClose(); }}
          >
            <span className="text-blue-400">&#x2398;</span>
            Duplicate
          </button>
          <div className="border-t border-gray-700 my-1" />
          <button
            className="w-full text-left px-3 py-1.5 hover:bg-red-900/50 text-xs text-red-400 flex items-center gap-2"
            onClick={() => { onDelete(); onClose(); }}
          >
            <span>&#x2716;</span>
            Delete
          </button>
        </>
      )}
      {isTerminal && (
        <div className="px-3 py-1.5 text-xs text-gray-500">
          Terminal node (no actions)
        </div>
      )}
    </div>
  );
}
