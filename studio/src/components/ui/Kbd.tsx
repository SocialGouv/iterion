import type { ReactNode } from "react";

export type KbdSize = "sm" | "md";

export interface KbdProps {
  children: ReactNode;
  /** `sm` (default) for dense legends; `md` for prominent shortcut dialogs. */
  size?: KbdSize;
  className?: string;
}

const sizeClass: Record<KbdSize, string> = {
  sm: "px-1.5 py-0.5 text-caption",
  md: "px-2 py-0.5 text-body",
};

/**
 * Kbd — a keyboard key-cap chip. Single source of truth for the `font-mono`
 * bordered chip the studio uses for shortcut hints (⌘K, ↑↓, esc, c, ?, …),
 * which had drifted into several hand-rolled inline copies with slightly
 * different padding/sizes. `align-middle` keeps it seated correctly when used
 * inline inside flowing text. Pass `className` to add non-conflicting
 * utilities (e.g. `shrink-0`, `whitespace-nowrap`).
 */
export function Kbd({ children, size = "sm", className = "" }: KbdProps) {
  return (
    <kbd
      className={`inline-flex items-center justify-center rounded border border-border-default bg-surface-2 align-middle font-mono text-fg-muted ${sizeClass[size]} ${className}`.trim()}
    >
      {children}
    </kbd>
  );
}
