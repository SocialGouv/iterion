import { formatContextUsage } from "@/lib/format";

// Thin progress bar showing how full the LLM context window got at
// the busiest moment of a node's session. Returns null when the
// backend (or its upstream proxy) didn't surface the window, so the
// canvas stays quiet rather than showing a misleading empty gauge.

interface Props {
  used: number | undefined;
  window: number | undefined;
  className?: string;
}

export function ContextUsageBar({ used, window, className }: Props) {
  const usage = formatContextUsage(used, window);
  if (!usage) return null;
  const tone =
    usage.pct >= 90
      ? { fill: "bg-danger/80", bg: "bg-danger/20" }
      : usage.pct >= 75
      ? { fill: "bg-warning/80", bg: "bg-warning/20" }
      : { fill: "bg-accent/80", bg: "bg-accent/20" };
  return (
    <div
      // A bar with a text alternative: aria-label on a roleless div is
      // prohibited (axe aria-prohibited-attr). Not a progressbar — it would
      // owe aria-valuenow/min/max, and the title already carries the figure.
      role="img"
      className={`mt-1 h-[3px] w-full rounded-sm overflow-hidden ${tone.bg} ${className ?? ""}`}
      title={usage.title}
      aria-label={usage.title}
    >
      <div
        className={`h-full ${tone.fill} transition-[width] duration-300`}
        style={{ width: `${usage.pct}%` }}
      />
    </div>
  );
}
