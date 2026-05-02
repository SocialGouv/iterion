import { useCallback, useMemo, useRef, useState, type CSSProperties, type UIEvent } from "react";
import { RefAwareTextarea } from "@/components/ui/RefAwareInput";
import PromptOverlayHighlight from "@/components/ui/PromptOverlayHighlight";
import type { RefContext } from "@/lib/refCompletion";

interface Props {
  value: string;
  onChange: (v: string) => void;
  refContext?: RefContext;
  rows?: number;
  placeholder?: string;
  autoFocus?: boolean;
  /** Render the editor at full available height instead of a fixed row count. */
  fillHeight?: boolean;
}

/**
 * Comfortable, prompt-first body editor: monospace textarea with an
 * overlay that highlights `{{...}}` template references and `${...}`
 * env vars. Used by the in-modal large editor and the in-Inspector
 * single-item edit view (`InspectorEditItem` with kind="prompt").
 *
 * The textarea text is rendered transparent so the overlay shows
 * through; the caret stays visible via `caret-color`. Both layers
 * share an identical inline style block so glyph metrics line up
 * regardless of Tailwind utility ordering.
 */
export default function PromptBodyEditor({
  value,
  onChange,
  refContext,
  rows = 16,
  placeholder,
  autoFocus,
  fillHeight = false,
}: Props) {
  const lineCount = useMemo(() => value.split("\n").length, [value]);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const [scroll, setScroll] = useState({ top: 0, left: 0 });

  const syncOverlayScroll = useCallback((el: HTMLTextAreaElement) => {
    setScroll((prev) => {
      if (prev.top === el.scrollTop && prev.left === el.scrollLeft) return prev;
      return { top: el.scrollTop, left: el.scrollLeft };
    });
  }, []);

  const handleScroll = useCallback(
    (e: UIEvent<HTMLTextAreaElement>) => {
      syncOverlayScroll(e.currentTarget);
    },
    [syncOverlayScroll],
  );

  // Inline style block shared by the overlay and the textarea so
  // glyph positions line up exactly. Inline styles win over Tailwind
  // utilities, which dodges any class-ordering surprises.
  const sharedBox: CSSProperties = {
    fontFamily:
      "ui-monospace, SFMono-Regular, Menlo, Monaco, 'Cascadia Code', 'Roboto Mono', Consolas, monospace",
    fontSize: 13,
    lineHeight: 1.55,
    letterSpacing: 0,
    padding: "8px 12px",
    margin: 0,
    boxSizing: "border-box",
    whiteSpace: "pre-wrap",
    wordBreak: "break-word",
    tabSize: 2,
  };

  const overlayStyle: CSSProperties = {
    ...sharedBox,
    color: "var(--color-fg-default)",
  };

  const textareaStyle: CSSProperties = {
    ...sharedBox,
    color: "transparent",
    caretColor: "var(--color-fg-default)",
    background: "transparent",
    border: "0",
    outline: "none",
    width: "100%",
    height: fillHeight ? "100%" : undefined,
    resize: fillHeight ? "none" : "vertical",
    position: "relative",
    zIndex: 1,
  };

  const wrapperStyle: CSSProperties = fillHeight
    ? { height: "100%", minHeight: 0 }
    : {};

  return (
    <div className="flex flex-col" style={wrapperStyle}>
      <div className="relative flex-1 rounded-md border border-border-strong bg-surface-1 focus-within:border-accent focus-within:ring-1 focus-within:ring-accent overflow-hidden">
        <PromptOverlayHighlight
          value={value}
          style={overlayStyle}
          scrollTop={scroll.top}
          scrollLeft={scroll.left}
        />
        <RefAwareTextarea
          ref={textareaRef}
          value={value}
          onChange={(next) => {
            onChange(next);
            const el = textareaRef.current;
            if (el) window.requestAnimationFrame(() => syncOverlayScroll(el));
          }}
          onScroll={handleScroll}
          rows={fillHeight ? undefined : rows}
          placeholder={placeholder}
          spellCheck={false}
          autoFocus={autoFocus}
          refContext={refContext ?? { kind: "prompt-body" }}
          className=""
          style={textareaStyle}
        />
      </div>
      <div className="mt-1 flex items-center justify-between text-[10px] text-fg-subtle">
        <span>
          {value.length} chars · {lineCount} {lineCount === 1 ? "line" : "lines"}
        </span>
        <span className="font-mono">
          {"{{...}}"} = template ref · {"${...}"} = env var
        </span>
      </div>
    </div>
  );
}
