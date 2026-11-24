import type { FocusEvent } from "react";

// ClampText is the one place text gets clamped (RGAA 10.12): the clamped
// render never puts the full text out of reach. Clipping is visual, so the
// whole string stays in the accessibility tree — as the block's own text,
// announced once. A keyboard user recovers it visually by focusing the
// standalone block (no fixed height cuts it).
//
// Inside a control (a button, a link) the block must not be a second tab
// stop for one control: pass `focusable={false}` there — the clamp is then
// purely visual, and the control's own accessible name carries the text
// (#2803). The source guard in __tests__/a11y/jsx-content.test.ts holds
// every nested call site to that opt-out.
//
// The raw `truncate`/`line-clamp-N` utilities on user-facing text are
// refused by the source-discipline clamp guard; route clamped text here.
export function ClampText({
  text,
  lines = 1,
  className = "",
  as: Tag = "span",
  focusable = true,
}: {
  text: string;
  /** How many lines to show before the ellipsis (line-clamp-N). */
  lines?: number;
  className?: string;
  as?: "span" | "p" | "div";
  /** False inside a control: no focus stop of its own — the control is the
   *  one stop, and its name carries the text (#2803). */
  focusable?: boolean;
}) {
  const reveal = (e: FocusEvent<HTMLElement>) => {
    // The inline style is SNAPSHOT, not mirrored property by property:
    // re-deriving it on blur means three places that must agree, and two
    // of them drifted — a `display` left on a single-line clamp and a
    // `white-space` left on a multi-line one, each surviving the blur for
    // good and breaking the block's layout from then on.
    const el = e.currentTarget;
    el.dataset.clampStyle = el.style.cssText;
    // Only the property the clamp actually set: a multi-line clamp wraps
    // already, and writing `normal` over an inherited `pre-wrap` would
    // collapse the newlines of the very text the focus just revealed.
    if (lines === 1) el.style.whiteSpace = "normal";
    el.style.overflow = "visible";
    el.style.display = "block";
    // The clamp itself, not only the box model it was declared in:
    // `-webkit-line-clamp` is no longer inert outside `-webkit-box`
    // (Chromium maps it onto the standard line-clamp for block
    // containers), so leaving it behind would reveal nothing.
    el.style.webkitLineClamp = "";
    el.style.webkitBoxOrient = "";
  };
  const restore = (e: FocusEvent<HTMLElement>) => {
    const el = e.currentTarget;
    el.style.cssText = el.dataset.clampStyle ?? "";
  };
  return (
    <Tag
      tabIndex={focusable ? 0 : undefined}
      data-clamp-text=""
      // The one attribute that identifies this primitive's node to a net
      // judging a surface that embeds it: where the block IS the focus
      // stop, the surface waives focus-order-semantics on THIS attribute
      // rather than on itself (axe focus-order-semantics is wrong about a
      // roleless focusable that exists to reveal the clamp —
      // CLAMP_TEXT_WAIVER); a nested, non-focusable block needs no waiver
      // and stays marked all the same.
      className={`min-w-0 ${className}`}
      style={
        lines > 1
          ? { display: "-webkit-box", WebkitBoxOrient: "vertical", WebkitLineClamp: lines, overflow: "hidden" }
          : { overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }
      }
      onFocus={focusable ? reveal : undefined}
      onBlur={focusable ? restore : undefined}
    >
      {text}
    </Tag>
  );
}
