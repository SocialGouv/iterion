import { useId } from "react";

// ClampText is the one place text gets clamped (RGAA 10.12): the clamped
// render never puts the full text out of reach. Clipping is visual, so the
// whole string stays in the accessibility tree, and a keyboard user recovers
// it visually by focusing the block (no fixed height cuts it).
//
// The raw `truncate`/`line-clamp-N` utilities on user-facing text are
// refused by the source-discipline clamp guard; route clamped text here.
export function ClampText({
  text,
  lines = 1,
  className = "",
  as: Tag = "span",
}: {
  text: string;
  /** How many lines to show before the ellipsis (line-clamp-N). */
  lines?: number;
  className?: string;
  as?: "span" | "p" | "div";
}) {
  const tipId = useId();
  return (
    <Tag
      tabIndex={0}
      aria-describedby={tipId}
      // The one attribute that identifies this primitive's node to a net
      // judging a surface that embeds it: the focus stop is deliberate
      // (axe focus-order-semantics is wrong about it), and a surface waives
      // the rule on THIS attribute rather than on itself.
      data-clamp-text=""
      className={`min-w-0 ${className}`}
      style={
        lines > 1
          ? { display: "-webkit-box", WebkitBoxOrient: "vertical", WebkitLineClamp: lines, overflow: "hidden" }
          : { overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }
      }
      onFocus={(e) => {
        // The inline style is SNAPSHOT, not mirrored property by property:
        // re-deriving it on blur means three places that must agree, and two
        // of them drifted — a `display` left on a single-line clamp and a
        // `white-space` left on a multi-line one, each surviving the blur for
        // good and breaking the block's layout from then on.
        const el = e.currentTarget as HTMLElement;
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
      }}
      onBlur={(e) => {
        const el = e.currentTarget as HTMLElement;
        el.style.cssText = el.dataset.clampStyle ?? "";
      }}
    >
      {text}
      <span id={tipId} className="sr-only">
        {text}
      </span>
    </Tag>
  );
}
