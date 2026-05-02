// Visual indicator for the LLM reasoning_effort field. Renders the level
// name plus a 3-cell intensity bar tinted by severity, so users can scan
// the canvas and tell low/medium/high apart without reading.

export type EffortLevel = "low" | "medium" | "high" | "extra_high";

export function isEffortLevel(s: string | undefined): s is EffortLevel {
  return s === "low" || s === "medium" || s === "high" || s === "extra_high";
}

interface Props {
  level: EffortLevel;
  // Show "live" badge when the runtime override differs from the
  // declared value. The bar itself reflects the active (override or
  // declared) level; this flag just appends the marker.
  live?: boolean;
  className?: string;
  title?: string;
}

const FILLED: Record<EffortLevel, number> = {
  low: 1,
  medium: 2,
  high: 3,
  // extra_high also fills 3 cells but uses the danger tint to push past
  // the visual ceiling of "high".
  extra_high: 3,
};

const TONE: Record<EffortLevel, { text: string; bar: string; cell: string }> = {
  low: {
    text: "text-fg-muted",
    bar: "bg-fg-muted/30",
    cell: "bg-fg-muted",
  },
  medium: {
    text: "text-accent",
    bar: "bg-accent/20",
    cell: "bg-accent",
  },
  high: {
    text: "text-warning-fg",
    bar: "bg-warning/20",
    cell: "bg-warning",
  },
  extra_high: {
    text: "text-danger-fg",
    bar: "bg-danger/20",
    cell: "bg-danger",
  },
};

export function EffortBar({ level, live, className, title }: Props) {
  const filled = FILLED[level];
  const tone = TONE[level];
  const label = level === "extra_high" ? "extra" : level;
  return (
    <span
      className={`inline-flex items-center gap-1 text-[9px] leading-none ${tone.text} ${className ?? ""}`}
      title={title ?? `reasoning_effort: ${level}`}
    >
      <span>{label}</span>
      <span className={`inline-flex gap-px rounded-sm p-px ${tone.bar}`}>
        {[0, 1, 2].map((i) => (
          <span
            key={i}
            className={`inline-block w-[3px] h-[6px] rounded-[1px] ${
              i < filled ? tone.cell : "bg-transparent"
            }`}
          />
        ))}
      </span>
      {live && (
        <span
          className="ml-0.5 px-1 rounded bg-info-soft text-info-fg text-[8px] uppercase"
          title="overridden at runtime"
        >
          live
        </span>
      )}
    </span>
  );
}
