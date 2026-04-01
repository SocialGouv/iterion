import { useEffect } from "react";

interface Props {
  open: boolean;
  onClose: () => void;
}

const SHORTCUTS = [
  { keys: "Ctrl+Z", desc: "Undo" },
  { keys: "Ctrl+Y / Ctrl+Shift+Z", desc: "Redo" },
  { keys: "Ctrl+S", desc: "Save" },
  { keys: "Delete / Backspace", desc: "Delete selected node or edge" },
  { keys: "Right-click node", desc: "Context menu (set entry, duplicate, delete)" },
  { keys: "/", desc: "Search nodes" },
  { keys: "Escape", desc: "Clear selection / close dialogs" },
  { keys: "?", desc: "Show this help" },
];

export default function ShortcutsHelp({ open, onClose }: Props) {
  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" onClick={onClose}>
      <div
        className="bg-gray-800 border border-gray-600 rounded-lg p-5 min-w-[350px] max-w-[450px]"
        onClick={(e) => e.stopPropagation()}
      >
        <h3 className="text-sm font-bold text-white mb-4">Keyboard Shortcuts</h3>
        <div className="space-y-2">
          {SHORTCUTS.map(({ keys, desc }) => (
            <div key={keys} className="flex items-center justify-between gap-4">
              <span className="text-xs text-gray-300">{desc}</span>
              <kbd className="bg-gray-700 border border-gray-600 rounded px-2 py-0.5 text-[10px] text-gray-200 font-mono whitespace-nowrap">
                {keys}
              </kbd>
            </div>
          ))}
        </div>
        <div className="mt-4 pt-3 border-t border-gray-700">
          <p className="text-[10px] text-gray-500 text-center">Press Escape to close</p>
        </div>
      </div>
    </div>
  );
}
