import { useMemo, type CSSProperties } from "react";
import { highlightPromptBody, type HighlightChunk } from "@/lib/promptHighlight";

interface Props {
  value: string;
  /** Tailwind classes — should match the textarea exactly (font, size, leading, padding). */
  className?: string;
  /** When true, no absolute positioning — render inline as a read-only preview. */
  inline?: boolean;
  /** Max height for the inline preview (`inline=true` only). */
  maxHeight?: string;
  /** Inline style passthrough — used to mirror the textarea's box-sizing. */
  style?: CSSProperties;
  /** Scroll offsets from the paired transparent textarea (overlay mode only). */
  scrollTop?: number;
  scrollLeft?: number;
}

/**
 * Renders prompt text with `{{...}}` template references and `${...}`
 * env vars styled. Used in two modes:
 *
 *  - **Overlay mode** (default): absolutely positioned underneath a
 *    transparent textarea, so user input shows the underlying tokens.
 *    The caller is responsible for matching font metrics on both
 *    layers and for passing textarea scroll offsets when the editor is
 *    scrollable.
 *  - **Inline mode** (`inline`): rendered as a normal flow element
 *    for read-only previews (e.g. the prompt picker preview strip).
 *
 * The component never mutates input — the concatenation of every
 * chunk text equals the input string exactly. This invariant is
 * verified by `promptHighlight.test.ts`.
 */
export default function PromptOverlayHighlight({
  value,
  className = "",
  inline = false,
  maxHeight,
  style,
  scrollTop = 0,
  scrollLeft = 0,
}: Props) {
  const chunks = useMemo(() => highlightPromptBody(value), [value]);

  const positionClass = inline
    ? ""
    : "pointer-events-none absolute inset-0 overflow-hidden whitespace-pre-wrap break-words";

  const baseStyle: CSSProperties = inline
    ? {
        whiteSpace: "pre-wrap",
        wordBreak: "break-word",
        maxHeight,
        overflow: maxHeight ? "hidden" : undefined,
        ...style,
      }
    : {
        // Mirror the textarea's text/caret behavior so chunks line up.
        whiteSpace: "pre-wrap",
        wordBreak: "break-word",
        ...style,
      };

  const content = (
    <>
      {chunks.length === 0 ? (
        // Keep one empty span so the overlay box still has a measurable
        // baseline (avoids 0-height layout collapse).
        <span>{"\u200B"}</span>
      ) : (
        chunks.map((c, i) => <Chunk key={i} chunk={c} />)
      )}
      {/* Trailing zero-width char so the overlay matches a textarea ending in \n */}
      <span>{"\u200B"}</span>
    </>
  );

  return (
    <div className={`${positionClass} ${className}`.trim()} aria-hidden="true" style={baseStyle}>
      {inline ? (
        content
      ) : (
        <div
          style={{
            transform: `translate(${-scrollLeft}px, ${-scrollTop}px)`,
            minWidth: "100%",
            willChange: "transform",
          }}
        >
          {content}
        </div>
      )}
    </div>
  );
}

function Chunk({ chunk }: { chunk: HighlightChunk }) {
  switch (chunk.kind) {
    case "ref":
      return (
        <span className="text-[var(--color-accent)] bg-[var(--color-accent-soft)] rounded-sm">
          {chunk.text}
        </span>
      );
    case "envvar":
      return (
        <span className="text-[var(--color-warning-fg)]">
          {chunk.text}
        </span>
      );
    case "comment":
      return <span className="text-fg-subtle italic">{chunk.text}</span>;
    case "text":
    default:
      return <span>{chunk.text}</span>;
  }
}
