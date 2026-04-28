import type { ReactNode } from "react";
import { useUIStore, type CanvasTool } from "@/store/ui";

const tools: { id: CanvasTool; label: string; shortcut: string; icon: ReactNode }[] = [
  {
    id: "select",
    label: "Select",
    shortcut: "V",
    icon: (
      <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round">
        <path d="M3 2l4 12 2-5 5-2L3 2z" />
      </svg>
    ),
  },
  {
    id: "pan",
    label: "Pan",
    shortcut: "H",
    icon: (
      <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
        <path d="M8 1v3M8 12v3M1 8h3M12 8h3M3.5 3.5l2 2M10.5 10.5l2 2M3.5 12.5l2-2M10.5 5.5l2-2" />
      </svg>
    ),
  },
];

export default function ToolPalette() {
  const canvasTool = useUIStore((s) => s.canvasTool);
  const setCanvasTool = useUIStore((s) => s.setCanvasTool);

  return (
    <div className="absolute top-14 left-2 z-40 flex flex-col gap-1 bg-surface-0/90 border border-border-default rounded-lg p-1">
      {tools.map((tool) => (
        <button
          key={tool.id}
          className={`flex items-center justify-center w-8 h-8 rounded transition-colors ${
            canvasTool === tool.id
              ? "bg-accent text-fg-default"
              : "text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
          }`}
          onClick={() => setCanvasTool(tool.id)}
          title={`${tool.label} (${tool.shortcut})`}
        >
          {tool.icon}
        </button>
      ))}
    </div>
  );
}
