export type LiveDotTone = "info" | "live" | "success" | "warning" | "danger" | "neutral";
export type LiveDotSize = "xs" | "sm" | "md";

export interface LiveDotProps {
  /** Semantic colour — info for "running", success for "data flowing /
   *  connected", warning for "reconnecting", danger for "disconnected". */
  tone?: LiveDotTone;
  size?: LiveDotSize;
  /** When false, render the dot statically (no pulse). Useful for steady
   *  states like a connected status that doesn't need to draw the eye. */
  pulse?: boolean;
  className?: string;
  /** Status label, rendered as sr-only TEXT inside the dot — not an
   *  aria-label, which paints no pixels and vanishes with the
   *  stylesheets (RGAA 10.2). sr-only keeps the text audible with CSS
   *  on and VISIBLE with CSS off, so the state survives both. Pass
   *  when the dot is the only visual signal of the state — omit when
   *  an adjacent text label already names it. */
  label?: string;
}

const toneClass: Record<LiveDotTone, string> = {
  info: "bg-info",
  // "live" is the dedicated secondary accent for "currently running"
  // — distinct from info (informational/neutral). See
  // docs/visual-identity.md § Secondary accent.
  live: "bg-live",
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-danger",
  neutral: "bg-fg-subtle",
};

const sizeClass: Record<LiveDotSize, string> = {
  xs: "w-1 h-1",
  sm: "w-1.5 h-1.5",
  md: "w-2 h-2",
};

/**
 * Small coloured "heartbeat" dot. Use to indicate that *something* is
 * live, active, or in flight — e.g. an active run, a WebSocket
 * connection, a streaming follow-live cursor. The tone disambiguates
 * the meaning: info = workflow active, success = data flowing,
 * warning = degraded, danger = severed.
 *
 * Not for skeleton-style shimmer (use `Skeleton`), urgent-attention
 * full badges (apply `animate-pulse` directly on the badge), or the
 * AI "thinking" glyph in ThinkingFooter (intentional bespoke).
 */
export function LiveDot({
  tone = "info",
  size = "sm",
  pulse = true,
  className = "",
  label,
}: LiveDotProps) {
  return (
    <span
      role={label ? "status" : undefined}
      aria-hidden={label ? undefined : true}
      className={`inline-block rounded-full ${toneClass[tone]} ${sizeClass[size]} ${pulse ? "animate-pulse" : ""} ${className}`.trim()}
    >
      {label ? <span className="sr-only">{label}</span> : null}
    </span>
  );
}
