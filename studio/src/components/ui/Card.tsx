import { forwardRef, type HTMLAttributes } from "react";

export interface CardProps extends HTMLAttributes<HTMLDivElement> {
  /**
   * Adds the hover affordance (subtle lift + stronger border + deeper shadow)
   * for cards that are themselves clickable (marketplace entries, run rows,
   * recent files). Pair with an `onClick` / wrapping link. Honours
   * prefers-reduced-motion via the global reset in app.css.
   */
  interactive?: boolean;
  /** Drop the default padding (for cards that manage their own inner layout). */
  flush?: boolean;
}

/**
 * Card — the canonical raised container. Encodes the studio's elevation
 * language so panels and list entries stop hand-rolling
 * `border bg-surface-1 rounded` with inconsistent radii/shadows. Resting state
 * is a hairline border + the lightest shadow token; `interactive` adds the
 * hover lift used across browse/list surfaces. See docs/design-system.md
 * § Elevation & cards.
 */
export const Card = forwardRef<HTMLDivElement, CardProps>(function Card(
  { interactive = false, flush = false, className = "", ...rest },
  ref,
) {
  const base =
    "rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)]";
  const pad = flush ? "" : "p-4";
  const hover = interactive
    ? "transition-[box-shadow,transform,border-color] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:-translate-y-0.5 hover:border-border-strong hover:shadow-[var(--shadow-md)]"
    : "";
  return (
    <div
      ref={ref}
      className={`${base} ${pad} ${hover} ${className}`.replace(/\s+/g, " ").trim()}
      {...rest}
    />
  );
});
