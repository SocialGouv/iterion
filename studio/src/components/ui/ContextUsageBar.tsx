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
      // A gauge, announced as one (ui/Meter does the same): aria-label on a
      // roleless div is prohibited ARIA, and role="img" would hide the value
      // assistive tech can otherwise report and compare.
      role="progressbar"
      aria-valuemin={0}
      // Both from the formatter, which owns the clamp the percentage already
      // applies: a valuenow above valuemax is an out-of-range gauge.
      aria-valuemax={usage.max}
      aria-valuenow={usage.now}
      aria-valuetext={usage.title}
      className={`mt-1 h-[3px] w-full rounded-sm overflow-hidden ${tone.bg} ${className ?? ""}`}
      title={usage.title}
      // The name says WHICH gauge, the value text says how full: the same
      // sentence in both is read twice, around the role.
      aria-label="Context usage"
    >
      <div
        className={`h-full ${tone.fill} transition-[width] duration-300`}
        style={{ width: `${usage.pct}%` }}
      />
    </div>
  );
}
