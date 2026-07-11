import { useState } from "react";

import { IconButton } from "@/components/ui";

import { statusClasses, type UnifiedStatus } from "../runStatusClasses";

// StatusLegend explains the canvas node colour palette. Collapsed by
// default to keep the run viewport uncluttered — the "?" toggle in the
// bottom-left expands a small card showing each status with its colour
// chip and the matching label, so first-time viewers can map "red
// border" → "failed" without having to dig through docs.
export function StatusLegend() {
  const [open, setOpen] = useState(false);
  const entries: Array<{ key: UnifiedStatus; sample: string }> = [
    { key: "running", sample: "bg-info-soft border-info" },
    { key: "finished", sample: "bg-success-soft border-success/60" },
    { key: "failed", sample: "bg-danger-soft border-danger/60" },
    { key: "paused_waiting_human", sample: "bg-warning-soft border-warning/60" },
    { key: "skipped", sample: "bg-surface-2 border-border-default" },
    { key: "none", sample: "bg-surface-1 border-border-default" },
  ];
  return (
    <div className="absolute bottom-2 left-2 z-[var(--z-canvas)]">
      {open ? (
        <div className="bg-surface-1/95 backdrop-blur border border-border-default rounded shadow-[var(--shadow-popover)] p-2 min-w-[180px]">
          <div className="flex items-center justify-between gap-2 mb-1">
            <span className="text-caption font-semibold text-fg-default">
              Node colours
            </span>
            <IconButton
              label="Hide node-colour legend"
              size="sm"
              variant="ghost"
              onClick={() => setOpen(false)}
            >
              ×
            </IconButton>
          </div>
          <ul className="space-y-0.5">
            {entries.map((e) => {
              const meta = statusClasses(e.key);
              return (
                <li key={e.key} className="flex items-center gap-2 text-caption">
                  <span
                    aria-hidden
                    className={`inline-block h-2.5 w-3.5 rounded border ${e.sample}`}
                  />
                  <span className="text-fg-default">{meta.label}</span>
                </li>
              );
            })}
          </ul>
        </div>
      ) : (
        <IconButton
          label="Show node-colour legend"
          size="sm"
          variant="secondary"
          onClick={() => setOpen(true)}
          className="bg-surface-1/90 backdrop-blur"
        >
          ?
        </IconButton>
      )}
    </div>
  );
}
