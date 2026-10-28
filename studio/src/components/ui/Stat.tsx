import type { ReactNode } from "react";

import { LiveDot } from "./LiveDot";

export type StatTone =
  | "default"
  | "info"
  | "warning"
  | "danger"
  | "success"
  | "live";
export type StatSize = "sm" | "md" | "lg";

export interface StatProps {
  /** Quiet caption label placed before (row) or under (stack) the value. */
  label: string;
  /** Pre-formatted value — formatCost / formatMs / formatTokens / a count. */
  value: ReactNode;
  /** Semantic emphasis for the value; default keeps it neutral. */
  tone?: StatTone;
  /** Trailing pulsing LiveDot — for a figure that is still ticking. */
  live?: boolean;
  /** Tooltip / title on hover. */
  hint?: string;
  size?: StatSize;
  /** `row` = inline "label value"; `stack` = value over label (a tile). */
  align?: "row" | "stack";
  /** Makes the Stat an activatable button (e.g. jump-to-failed node). */
  onClick?: () => void;
  /** Overrides the accessible name (defaults to the visible label+value). */
  ariaLabel?: string;
  className?: string;
}

// Value-emphasis tone. Every entry is a real --color-*-fg token (see
// app.css); kept as a static map so Tailwind's JIT sees each class.
const valueToneClass: Record<StatTone, string> = {
  default: "text-fg-default",
  info: "text-info-fg",
  warning: "text-warning-fg",
  danger: "text-danger-fg",
  success: "text-success-fg",
  live: "text-live-fg",
};

const sizeClass: Record<StatSize, { label: string; value: string }> = {
  sm: { label: "text-caption", value: "text-micro" },
  md: { label: "text-caption", value: "text-body" },
  lg: { label: "text-caption uppercase tracking-wide", value: "text-title" },
};

// Stat is the run console's atomic "label + value" readout — a quiet
// caption label with a monospaced, semibold value carrying an optional
// semantic tone and a trailing LiveDot for still-ticking figures.
// Extracted from RunMetrics' local Metric so the header vitals strip and
// the Overview meters/counters speak one visual language.
export function Stat({
  label,
  value,
  tone = "default",
  live = false,
  hint,
  size = "sm",
  align = "row",
  onClick,
  ariaLabel,
  className = "",
}: StatProps) {
  const sz = sizeClass[size];
  const stacked = align === "stack";

  const labelEl = <span className={`text-fg-subtle ${sz.label}`}>{label}</span>;
  const valueEl = (
    <span
      className={`font-mono font-semibold ${sz.value} ${valueToneClass[tone]}`}
    >
      {value}
      {live && <LiveDot tone="live" size="xs" className="ml-1 align-middle" />}
    </span>
  );

  const inner = stacked ? (
    <>
      {valueEl}
      {labelEl}
    </>
  ) : (
    <>
      {labelEl}
      {valueEl}
    </>
  );

  const layout = stacked
    ? "inline-flex flex-col items-start gap-0.5"
    : "inline-flex items-baseline gap-1";

  if (onClick) {
    return (
      <button
        type="button"
        onClick={onClick}
        title={hint}
        aria-label={ariaLabel}
        className={`${layout} rounded px-0.5 hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent ${className}`.trim()}
      >
        {inner}
      </button>
    );
  }

  return (
    <span
      className={`${layout} ${className}`.trim()}
      title={hint}
      aria-label={ariaLabel}
    >
      {inner}
    </span>
  );
}
