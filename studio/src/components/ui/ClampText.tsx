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
        (e.currentTarget as HTMLElement).style.whiteSpace = "normal";
        (e.currentTarget as HTMLElement).style.overflow = "visible";
        (e.currentTarget as HTMLElement).style.display = "block";
      }}
      onBlur={(e) => {
        // Every property onFocus wrote is restored, `display` included: a
        // single-line clamp has none of its own, and leaving the focused
        // `block` behind breaks the line of an inline <span> for good.
        (e.currentTarget as HTMLElement).style.whiteSpace = lines > 1 ? "normal" : "nowrap";
        (e.currentTarget as HTMLElement).style.overflow = "hidden";
        (e.currentTarget as HTMLElement).style.display = lines > 1 ? "-webkit-box" : "";
      }}
    >
      {text}
      <span id={tipId} className="sr-only">
        {text}
      </span>
    </Tag>
  );
}
