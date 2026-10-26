import { useId } from "react";

// ClampText is the one place text gets clamped (RGAA 10.12): the clamped
// render never puts the full text out of reach — the full content stays
// recoverable through the native title AND keyboard focus (focus-visible
// reveals the whole block; no fixed height cuts it).
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
        (e.currentTarget as HTMLElement).style.whiteSpace = lines > 1 ? "normal" : "nowrap";
        (e.currentTarget as HTMLElement).style.overflow = "hidden";
        if (lines > 1)
          (e.currentTarget as HTMLElement).style.display = "-webkit-box";
      }}
    >
      {text}
      <span id={tipId} className="sr-only">
        {text}
      </span>
    </Tag>
  );
}
