// Visual indicator for the LLM reasoning_effort field. Renders the level
// name plus a 5-cell intensity bar tinted by severity, so users can scan
// the canvas and tell none/low/medium/high/xhigh/max/ultracode apart without reading.
// "none" (no reasoning — GPT-6 Sol/Luna) renders an empty bar.
// "ultracode" is a mode (xhigh + workflow orchestration), shown full-bar in a
// distinct accent tone so it reads as "beyond max".

export type EffortLevel = "none" | "low" | "medium" | "high" | "xhigh" | "max" | "ultracode";

export function isEffortLevel(s: string | undefined): s is EffortLevel {
  return (
    s === "none" ||
    s === "low" ||
    s === "medium" ||
    s === "high" ||
    s === "xhigh" ||
    s === "max" ||
    s === "ultracode"
  );
}

interface Props {
  level: EffortLevel;
  // Show "live" badge when the runtime override differs from the
  // declared value. The bar itself reflects the active (override or
  // declared) level; this flag just appends the marker.
  live?: boolean;
  // Render in attenuated style when the level was resolved from the
  // provider's documented default rather than declared in the workflow
  // or set at runtime. Lets the user distinguish "I chose this" from
  // "this is what the provider would use anyway".
  muted?: boolean;
  // Model's supported effort levels (low→high). When provided the
  // bar normalises to this range — none excluded, it is no rung — so
  // the model's top level always fills every cell — gpt-5 at "high"
  // renders 4/4 instead of 3/5.
  // Falls back to the global low/medium/high/xhigh/max scale when
  // omitted or when level isn't in the list.
  supported?: string[];
  className?: string;
  title?: string;
}

const FILLED: Record<EffortLevel, number> = {
  none: 0,
  low: 1,
  medium: 2,
  high: 3,
  xhigh: 4,
  max: 5,
  ultracode: 5,
};

const TONE: Record<EffortLevel, { text: string; bar: string; cell: string }> = {
  none: {
    text: "text-fg-muted",
    bar: "bg-fg-muted/20",
    cell: "bg-fg-muted/60",
  },
  low: {
    text: "text-fg-muted",
    bar: "bg-fg-muted/30",
    cell: "bg-fg-muted",
  },
  medium: {
    text: "text-accent-text",
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
  ultracode: {
    text: "text-accent-text",
    bar: "bg-accent/20",
    cell: "bg-accent",
  },
};

export function EffortBar({ level, live, muted, supported, className, title }: Props) {
  const tone = TONE[level];
  // Normalise to the model's supported range when known; "high" on a
  // 4-level model fills 4/4. Else use the global low→max scale.
  // none is the absence of reasoning, not a rung: it never occupies a cell
  // of the normalised scale, so a model's lowest real level fills exactly
  // one cell whether or not the model also carries none (pi, GPT-6
  // Sol/Luna) — and none itself draws that same scale empty.
  const scale = supported ? supported.filter((s) => s !== "none") : [];
  const scaleIdx = scale.indexOf(level);
  const normalised = scaleIdx >= 0 || (level === "none" && scale.length > 0 && !!supported?.includes("none"));
  const total = normalised ? scale.length : 5;
  const filled = level === "none" ? 0 : scaleIdx >= 0 ? scaleIdx + 1 : FILLED[level];
  const cells = Array.from({ length: total }, (_, i) => i);
  const isModelMax = scaleIdx >= 0 && scaleIdx === total - 1;
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
          title="provider default — no value declared in workflow"
        >
          default
        </span>
      )}
    </span>
  );
}
