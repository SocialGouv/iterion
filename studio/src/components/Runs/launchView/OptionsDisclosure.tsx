// Shared collapsed-by-default disclosure shell for the launch form's two
// option groups ("Bot options" / "Engine options"). Everything inside is
// tuning, not launch-blocking — the collapsed header still says how much
// is tucked away via the option count.

import type { ReactNode } from "react";

import { Badge } from "@/components/ui/Badge";

export interface OptionsDisclosureProps {
  label: string;
  count: number;
  /** Muted summary of what's inside, shown next to the count. */
  hint: string;
  open: boolean;
  onToggle: () => void;
  children: ReactNode;
}

export default function OptionsDisclosure({
  label,
  count,
  hint,
  open,
  onToggle,
  children,
}: OptionsDisclosureProps) {
  return (
    <div className="border-t border-border-subtle pt-2">
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        className="flex w-full items-center gap-1.5 py-1 text-xs font-medium text-fg-muted hover:text-fg-default"
      >
        <span aria-hidden>{open ? "▾" : "▸"}</span>
        {label}
        <Badge variant="neutral" size="sm">
          {count} option{count === 1 ? "" : "s"}
        </Badge>
        <span className="font-normal text-fg-subtle">{hint}</span>
      </button>
      {open && children}
    </div>
  );
}
