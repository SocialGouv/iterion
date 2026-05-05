// Visual indicator for the LLM reasoning_effort field. Renders the level
// name plus a 5-cell intensity bar tinted by severity, so users can scan
// the canvas and tell low/medium/high/xhigh/max apart without reading.

export type EffortLevel = "low" | "medium" | "high" | "xhigh" | "max";

export function isEffortLevel(s: string | undefined): s is EffortLevel {
  return (
    s === "low" ||
    s === "medium" ||
    s === "high" ||
    s === "xhigh" ||
    s === "max"
  );
}

interface Props {
  level: EffortLevel;
  // Show "live" badge when the runtime override differs from the
  // declared value. The bar itself reflects the active (override or
  // declared) level; this flag just appends the marker.
  live?: boolean;
  // Render in attenuated style when the level was resolved from the
  // provider's documented default rather than declared in the .iter
  // or set at runtime. Lets the user distinguish "I chose this" from
  // "this is what the provider would use anyway".
  muted?: boolean;
  // Model's supported effort levels (low→high). When provided the
  // bar normalises to this range so the model's top level always
  // fills every cell — gpt-5 at "high" renders 4/4 instead of 3/5.
  // Falls back to the global low/medium/high/xhigh/max scale when
  // omitted or when level isn't in the list.
  supported?: string[];
  className?: string;
  title?: string;
}

const FILLED: Record<EffortLevel, number> = {
  low: 1,
  medium: 2,
  high: 3,
  xhigh: 4,
  max: 5,
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
  xhigh: {
    text: "text-danger-fg",
    bar: "bg-danger/20",
    cell: "bg-danger/70",
  },
  max: {
    text: "text-danger-fg",
    bar: "bg-danger/20",
    cell: "bg-danger",
  },
};

export function EffortBar({ level, live, muted, supported, className, title }: Props) {
  const tone = TONE[level];
  // Normalise to the model's supported range when known; "high" on a
  // 4-level model fills 4/4. Else use the global low→max scale.
  const supportedIdx = supported ? supported.indexOf(level) : -1;
  const total = supported && supportedIdx >= 0 ? supported.length : 5;
  const filled = supportedIdx >= 0 ? supportedIdx + 1 : FILLED[level];
  const cells = Array.from({ length: total }, (_, i) => i);
  const isModelMax = supportedIdx >= 0 && supportedIdx === total - 1;
  const defaultTitle = muted
    ? `reasoning_effort: ${level} (provider default)`
    : isModelMax
    ? `reasoning_effort: ${level} (model max)`
    : `reasoning_effort: ${level}`;
  return (
    <span
      className={`inline-flex items-center gap-1 text-[9px] leading-none ${tone.text} ${
        muted ? "opacity-60 italic" : ""
      } ${className ?? ""}`}
      title={title ?? defaultTitle}
    >
      <span>{level}</span>
      <span className={`inline-flex gap-px rounded-sm p-px ${tone.bar}`}>
        {cells.map((i) => (
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
      {muted && !live && (
        <span
          className="ml-0.5 text-[8px] uppercase tracking-wide"
          title="provider default — no value declared in .iter"
        >
          default
        </span>
      )}
    </span>
  );
}
