import type { ReactNode } from "react";

export interface KbdProps {
  children: ReactNode;
  className?: string;
}

/**
 * Kbd — a keyboard key-cap chip. Single source of truth for the
 * `font-mono` bordered chip the studio uses for shortcut hints (⌘K, ↑↓,
 * esc, …), which had drifted into several hand-rolled inline copies with
 * slightly different padding/surfaces. Pass `className` to tweak spacing
 * for a dense legend.
 */
export function Kbd({ children, className = "" }: KbdProps) {
  return (
    <kbd
      className={`inline-flex items-center justify-center rounded border border-border-default bg-surface-2 px-1.5 py-0.5 font-mono text-caption text-fg-muted ${className}`.trim()}
    >
      {children}
    </kbd>
  );
}
