import type { ReactNode } from "react";
import { TerminalCaret } from "./TerminalCaret";

export interface EmptyStateProps {
  // Body copy. Pass an empty string to render the centered slot
  // without any body text — useful as a neutral pre-fetch slate.
  message: ReactNode;
  // Optional headline above the message. When supplied, the layout
  // shifts to a richer composition (title → body → actions) that the
  // RunList / Board / Home views rely on for their hero empty states.
  title?: ReactNode;
  icon?: ReactNode;
  action?: ReactNode;
  // Optional secondary action rendered alongside `action`. Use when
  // the empty state has a second equally-weighted next step (e.g.
  // "Open Editor" + "Browse examples").
  secondaryAction?: ReactNode;
  // Opt into a gentle blinking terminal caret after the message — the
  // hacker-culture affordance for "waiting / nothing here yet" slates.
  caret?: boolean;
  className?: string;
}

// Empty/loading placeholder shared across list/tree panels. Keeps the
// vocabulary (centered, fg-subtle, small body text) consistent so list
// surfaces feel like one product rather than ten ad-hoc placeholders.
//
// Two layouts:
//   - Compact (default): icon · body · action. Used for inline panels.
//   - Rich (title supplied): title · body (fg-subtle) · actions row.
//     Lifts the hierarchy so high-traffic empties (zero runs, empty
//     backlog) feel like a deliberate landing surface, not a 404.
export function EmptyState({
  message,
  title,
  icon,
  action,
  secondaryAction,
  caret = false,
  className = "",
}: EmptyStateProps) {
  const hasActions = Boolean(action || secondaryAction);
  const hasMessage = message !== "";
  return (
    <div
      className={`flex h-full flex-col items-center justify-center gap-2 px-3 py-8 text-center text-xs text-fg-subtle ${className}`.trim()}
    >
      {icon &&
        (title ? (
          // Rich/hero empty state: seat the glyph in a soft elevated chip so
          // a zero-data landing reads as a deliberate surface, not a 404.
          <span
            aria-hidden
            className="mb-1 flex h-12 w-12 items-center justify-center rounded-[var(--radius-lg)] border border-border-default bg-surface-2 text-fg-subtle shadow-[var(--shadow-sm)]"
          >
            {icon}
          </span>
        ) : (
          <span aria-hidden>{icon}</span>
        ))}
      {title && (
        <div className="text-display font-semibold tracking-tight text-fg-default">
          {title}
        </div>
      )}
      {(hasMessage || caret) && (
        <div className={title ? "max-w-sm" : ""}>
          {message}
          {caret && <TerminalCaret className={hasMessage ? "ml-1" : ""} />}
        </div>
      )}
      {hasActions && (
        <div className="mt-2 flex items-center justify-center gap-2 flex-wrap">
          {action}
          {secondaryAction}
        </div>
      )}
    </div>
  );
}
