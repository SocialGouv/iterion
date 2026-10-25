import type {
  KeyboardEvent as ReactKeyboardEvent,
  PointerEvent as ReactPointerEvent,
  ReactNode,
} from "react";

// ResizeAxis is one resizable dimension of the panel a separator
// belongs to. `grow` names the arrow key that ENLARGES the panel —
// the direction the handle travels when the panel grows:
//   - "start" — the panel is pinned to the far edge, so growing moves
//     the handle against the axis (ArrowLeft widens a right-anchored
//     panel; ArrowUp heightens a bottom-anchored one);
//   - "end" — the panel is pinned to the near edge and grows with the
//     axis (ArrowRight widens a left-anchored panel).
export interface ResizeAxis {
  value: number;
  min: number;
  max: number;
  grow: "start" | "end";
  onResize: (next: number) => void;
}

// ResizeSeparator is the one resizable-edge primitive (RGAA 7.3): an
// ARIA window splitter that is tab-focusable, answers to the arrows
// (Shift = a big step), jumps to its bounds with Home/End, resets on
// double-click, and exposes the full aria-valuenow/min/max/valuetext
// set. It was previously written by hand five times — two handles with
// no keyboard support at all, two with divergent key maps, one
// complete (the pipeline drawer's, this primitive's reference).
//
// The pointer drag stays with the caller (onPointerResizeStart): every
// panel pins to a different edge, and three of them document why
// window-level listeners beat pointer capture for their shape. The
// keyboard/AT contract — the part a drag-only handle lacks — is owned
// here; callers only clamp/persist inside onResize. Handled keys are
// preventDefault-ed and stopPropagation-ed so a resize never travels
// to the page's own shortcut handlers.
export function ResizeSeparator({
  label,
  axes,
  step = 16,
  bigStep = 64,
  onReset,
  onPointerResizeStart,
  valuetext,
  className,
  title,
  children,
}: {
  /** Accessible name — which panel this handle resizes. */
  label: string;
  /** One axis for a single-direction handle, two for a corner grip
   *  (x = width via ←/→, y = height via ↑/↓). */
  axes: { x?: ResizeAxis; y?: ResizeAxis };
  step?: number;
  bigStep?: number;
  /** Double-click: back to the panel's default size, when it has one. */
  onReset?: () => void;
  /** The caller's pointer-drag bootstrap (window listeners etc.). */
  onPointerResizeStart?: (e: ReactPointerEvent<HTMLDivElement>) => void;
  /** aria-valuetext override; defaults to "N pixels wide/high" per axis. */
  valuetext?: string;
  className?: string;
  title?: string;
  /** A visible grip, when the handle carries one (a corner chevron). */
  children?: ReactNode;
}) {
  const { x, y } = axes;
  const primary: ResizeAxis | undefined = x ?? y;
  const axisCount = (x ? 1 : 0) + (y ? 1 : 0);

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const size = e.shiftKey ? bigStep : step;
    const bump = (axis: ResizeAxis, grows: boolean) => {
      e.preventDefault();
      e.stopPropagation();
      axis.onResize(
        Math.min(axis.max, Math.max(axis.min, axis.value + (grows ? size : -size))),
      );
    };
    if (x && e.key === "ArrowLeft") bump(x, x.grow === "start");
    else if (x && e.key === "ArrowRight") bump(x, x.grow === "end");
    else if (y && e.key === "ArrowUp") bump(y, y.grow === "start");
    else if (y && e.key === "ArrowDown") bump(y, y.grow === "end");
    else if (
      // Home/End need a single axis to mean anything; a corner grip
      // spanning both skips them.
      axisCount === 1 &&
      primary &&
      (e.key === "Home" || e.key === "End")
    ) {
      bump(primary, e.key === "End");
    }
  };

  return (
    <div
      role="separator"
      aria-label={label}
      aria-orientation={x && y ? undefined : x ? "vertical" : "horizontal"}
      aria-valuenow={primary ? Math.round(primary.value) : undefined}
      aria-valuemin={primary?.min}
      aria-valuemax={primary?.max}
      aria-valuetext={valuetext ?? defaultValuetext(axes)}
      tabIndex={0}
      onKeyDown={onKeyDown}
      onDoubleClick={onReset}
      onPointerDown={onPointerResizeStart}
      title={title}
      className={className}
    >
      {children}
    </div>
  );
}

function defaultValuetext(axes: { x?: ResizeAxis; y?: ResizeAxis }): string | undefined {
  const parts: string[] = [];
  if (axes.x) parts.push(`${Math.round(axes.x.value)} pixels wide`);
  if (axes.y) parts.push(`${Math.round(axes.y.value)} pixels high`);
  return parts.length > 0 ? parts.join(" × ") : undefined;
}
