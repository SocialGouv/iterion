export type MeterTone =
  | "accent"
  | "info"
  | "live"
  | "success"
  | "warning"
  | "danger"
  | "neutral";
export type MeterSize = "xs" | "sm" | "md";

export interface MeterProps {
  /** Caption label on the header row (left). Omit for a bare bar. */
  label?: string;
  /** Current value. */
  value: number;
  /** Ceiling. When omitted or 0 → "no cap" mode: a bare readout, no bar. */
  max?: number;
  /** Formats the numeric readouts (value and, unless overridden, max). */
  formatValue?: (v: number) => string;
  formatMax?: (v: number) => string;
  /** Auto-tone thresholds as ratios; default warning 0.75 / danger 0.9. */
  toneThresholds?: { warning?: number; danger?: number };
  /** Pin a tone regardless of ratio (e.g. a running loop is always live). */
  fixedTone?: MeterTone;
  size?: MeterSize;
  /** Tooltip / title + fallback accessible name. */
  hint?: string;
  /** Bar only — drop the label/value header row (inline usage). */
  compact?: boolean;
  className?: string;
}

// Fill + track colours per tone. Every class is a real --color-* token
// at a fixed opacity step; kept as static maps so Tailwind's JIT emits
// them (never string-interpolate `bg-${tone}/80` — the scanner can't see
// it and the bar renders with no colour).
const fillClass: Record<MeterTone, string> = {
  accent: "bg-accent/80",
  info: "bg-info/80",
  live: "bg-live/80",
  success: "bg-success/80",
  warning: "bg-warning/80",
  danger: "bg-danger/80",
  neutral: "bg-fg-subtle/70",
};
const trackClass: Record<MeterTone, string> = {
  accent: "bg-accent/20",
  info: "bg-info/20",
  live: "bg-live/20",
  success: "bg-success/20",
  warning: "bg-warning/20",
  danger: "bg-danger/20",
  neutral: "bg-surface-3",
};
const heightClass: Record<MeterSize, string> = {
  xs: "h-[3px]",
  sm: "h-1.5",
  md: "h-2",
};

// Meter is the run console's progress/usage bar: a labelled track with a
// tone-graded fill that warns and alarms as it fills. Consolidates the
// shapes of ContextUsageBar and the SessionBoard progress bar, and adds a
// "no cap" mode so a budget dimension whose ceiling isn't on the wire yet
// degrades gracefully to a bare readout instead of a misleading gauge.
export function Meter({
  label,
  value,
  max,
  formatValue = (v) => String(v),
  formatMax,
  toneThresholds,
  fixedTone,
  size = "sm",
  hint,
  compact = false,
  className = "",
}: MeterProps) {
  const hasCap = typeof max === "number" && max > 0;
  const fmtMax = formatMax ?? formatValue;

  // No-cap mode: a bare readout — the graceful degradation when the
  // backend hasn't sent a ceiling for this dimension.
  if (!hasCap) {
    return (
      <div className={className} title={hint}>
        {!compact && label ? (
          <div className="flex items-baseline justify-between gap-2">
            <span className="text-caption uppercase tracking-wide text-fg-subtle truncate">
              {label}
            </span>
            <span className="font-mono text-caption text-fg-default shrink-0">
              {formatValue(value)}
            </span>
          </div>
        ) : (
          <span className="font-mono text-caption text-fg-default">
            {formatValue(value)}
          </span>
        )}
      </div>
    );
  }

  const ratio = Math.min(1, Math.max(0, value / max));
  const warnAt = toneThresholds?.warning ?? 0.75;
  const dangerAt = toneThresholds?.danger ?? 0.9;
  const tone: MeterTone =
    fixedTone ??
    (ratio >= dangerAt ? "danger" : ratio >= warnAt ? "warning" : "accent");
  const pct = Math.round(ratio * 100);
  const a11yName = label ?? hint ?? "usage";

  return (
    <div className={className}>
      {!compact && (
        <div className="flex items-baseline justify-between gap-2 mb-1">
          {label && (
            <span className="text-caption uppercase tracking-wide text-fg-subtle truncate">
              {label}
            </span>
          )}
          <span className="font-mono text-caption text-fg-default shrink-0">
            {formatValue(value)}
            <span className="text-fg-subtle"> / {fmtMax(max)}</span>
          </span>
        </div>
      )}
      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={value}
        aria-label={a11yName}
        title={hint}
        className={`w-full overflow-hidden rounded-full ${heightClass[size]} ${trackClass[tone]}`}
      >
        <div
          className={`h-full rounded-full ${fillClass[tone]} transition-[width] duration-300`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}
