import { useId, useMemo, useState } from "react";

import { CopyButton } from "./CopyButton";

// ExpandableValue renders ONE potentially-long value — a multi-line prompt, a
// JSON blob, a final answer — without ever putting it permanently out of
// reach. Short values render whole; long ones collapse to a preview with a
// "Show all N lines" toggle that expands IN PLACE (no scroll box that only
// ever shows ten lines). Every value carries a copy button, and a value that
// is JSON gets a raw ⇄ pretty toggle.
//
// Shared by the pipeline card drawer (inputs + result) and the run console's
// node detail panel so both surfaces behave the same way.

// A value is "long" past either bound: the char count catches one giant
// unwrapped paragraph, the line count catches a tall but narrow JSON blob.
export const COLLAPSE_MAX_CHARS = 700;
export const COLLAPSE_MAX_LINES = 12;

export interface ValueRepresentations {
  /** The text shown by default — pretty-printed when the value is JSON. */
  pretty: string;
  /** The verbatim / compact form, or null when it equals `pretty`. */
  raw: string | null;
}

// valueRepresentations splits a value into what to show by default and what
// the raw toggle reveals. A JSON *string* (how bot_args carry structured
// values) keeps its verbatim text as `raw`; a structured value gets the
// single-line JSON as `raw`. Anything else has no second form, so the toggle
// does not render.
export function valueRepresentations(value: unknown): ValueRepresentations {
  if (value === null || value === undefined) return { pretty: "", raw: null };
  if (typeof value === "string") {
    const trimmed = value.trim();
    if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
      try {
        const parsed: unknown = JSON.parse(trimmed);
        if (parsed !== null && typeof parsed === "object") {
          const pretty = JSON.stringify(parsed, null, 2);
          return pretty === value ? { pretty, raw: null } : { pretty, raw: value };
        }
      } catch {
        // Not JSON after all — fall through to the plain-text rendering.
      }
    }
    return { pretty: value, raw: null };
  }
  if (typeof value !== "object") return { pretty: String(value), raw: null };
  try {
    const pretty = JSON.stringify(value, null, 2) ?? String(value);
    const compact = JSON.stringify(value) ?? pretty;
    return { pretty, raw: compact === pretty ? null : compact };
  } catch {
    // Circular / non-serialisable — better a lossy string than a crash.
    return { pretty: String(value), raw: null };
  }
}

export function countLines(text: string): number {
  return text.length === 0 ? 0 : text.split("\n").length;
}

export function isLongValue(text: string): boolean {
  return text.length > COLLAPSE_MAX_CHARS || countLines(text) > COLLAPSE_MAX_LINES;
}

export interface ExpandableValueProps {
  /** The value to render. Objects and JSON strings get the raw/pretty toggle. */
  value: unknown;
  /** Names the value in the copy/expand accessible labels (e.g. an input key). */
  label?: string;
  /**
   * Chrome around the text. "boxed" (default) draws the bordered surface box
   * the drawer uses; "bare" draws none, for callers that already provide their
   * own container (a <details> body, a card). Use this rather than fighting
   * the base classes from `className` — same-specificity Tailwind utilities
   * resolve by stylesheet order, not by class-attribute order.
   */
  variant?: "boxed" | "bare";
  /**
   * Extra classes on the text block. Keep to properties the base does not set
   * (spacing, width) — an override of a property it DOES set (colour, font
   * size, border) resolves by stylesheet order, not by class-attribute order,
   * so it silently may not win.
   */
  className?: string;
  /** Collapsed preview height, any CSS length. */
  collapsedMaxHeight?: string;
  /** Element to wrap in — `dd` when the caller is inside a <dl>. */
  as?: "div" | "dd";
  /** Start expanded (callers that already know the value is the point of the view). */
  defaultExpanded?: boolean;
}

export function ExpandableValue({
  value,
  label,
  variant = "boxed",
  className = "",
  collapsedMaxHeight = "12rem",
  as: Wrapper = "div",
  defaultExpanded = false,
}: ExpandableValueProps) {
  const { pretty, raw } = useMemo(() => valueRepresentations(value), [value]);
  const [showRaw, setShowRaw] = useState(false);
  const [expanded, setExpanded] = useState(defaultExpanded);
  const bodyId = useId();

  const text = showRaw && raw !== null ? raw : pretty;
  const long = isLongValue(text);
  const collapsed = long && !expanded;
  const suffix = label ? ` ${label}` : " value";

  return (
    <Wrapper className="group relative m-0 space-y-1">
      <div className="relative">
        <pre
          id={bodyId}
          style={collapsed ? { maxHeight: collapsedMaxHeight } : undefined}
          className={`m-0 whitespace-pre-wrap break-words font-mono text-xs text-fg-default ${
            variant === "boxed"
              ? "rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1"
              : ""
          } ${raw !== null ? "pr-14" : "pr-7"} ${
            // The accent underline is the "content continues below" cue — a
            // gradient fade would have to guess the caller's surface colour.
            collapsed ? "overflow-hidden border-b-2 border-b-accent/40" : ""
          } ${className}`}
        >
          {text}
        </pre>
        {/* Copy + raw/pretty ride the top-right corner. They stay in the DOM
            (and in the tab order) at all times — revealed on hover OR keyboard
            focus — so they are reachable without a pointer. */}
        <div className="absolute right-1 top-1 flex items-center gap-0.5 rounded bg-surface-1/90 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100">
          {raw !== null && (
            <button
              type="button"
              onClick={() => setShowRaw((v) => !v)}
              aria-pressed={showRaw}
              title={showRaw ? "Show pretty-printed JSON" : "Show the raw value"}
              aria-label={
                showRaw ? `Show pretty-printed${suffix}` : `Show raw${suffix}`
              }
              className="rounded px-1 text-micro text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
            >
              {showRaw ? "pretty" : "raw"}
            </button>
          )}
          <CopyButton value={text} variant="icon" label={`Copy${suffix}`} />
        </div>
      </div>
      {long && (
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          aria-expanded={expanded}
          aria-controls={bodyId}
          className="rounded px-1 text-micro text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
        >
          {expanded
            ? "Show less"
            : countLines(text) > 1
              ? `Show all ${countLines(text)} lines`
              : "Show more"}
        </button>
      )}
    </Wrapper>
  );
}
