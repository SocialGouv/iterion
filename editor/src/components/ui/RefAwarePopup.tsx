import { useEffect, useMemo, useState } from "react";
import { createPortal } from "react-dom";
import {
  computeRefs,
  fuzzyScore,
  groupRefs,
  type RefContext,
  type RefSuggestion,
} from "@/lib/refCompletion";
import { useCaretPosition } from "@/hooks/useCaretPosition";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";

const GROUP_LABELS: Record<string, string> = {
  input: "input",
  vars: "vars",
  outputs: "outputs",
  sessions: "sessions",
  artifacts: "artifacts",
};

interface TokenContext {
  /** Index of the `{{` opener in the value string. */
  start: number;
  /** Index just after the caret (exclusive end of the partial token). */
  caret: number;
  /** Text after the `{{`, used as the fuzzy query. */
  query: string;
}

/**
 * Detect whether the caret sits inside an unterminated `{{ ... }}` expression.
 * Returns null when no active token is present.
 */
function detectToken(value: string, caretIndex: number): TokenContext | null {
  // Walk backwards looking for the most recent `{{` with no intervening `}}`.
  let i = caretIndex - 1;
  while (i >= 1) {
    const ch = value[i]!;
    if (ch === "}" && value[i - 1] === "}") return null;
    if (ch === "{" && value[i - 1] === "{") {
      const start = i - 1;
      const query = value.slice(start + 2, caretIndex);
      // Whitespace or `}` cancels the trigger (multi-token expressions get
      // cancelled to avoid surprising behavior on existing valid templates).
      if (/[\s{}]/.test(query)) return null;
      return { start, caret: caretIndex, query };
    }
    i -= 1;
  }
  return null;
}

interface RefAwarePopupProps {
  /** The element whose value is being edited. */
  element: HTMLInputElement | HTMLTextAreaElement | null;
  /** The current value of the element. */
  value: string;
  /** Selection (caret) position into `value`. */
  caret: number;
  /** Reference context driving which refs are computed. */
  refContext: RefContext;
  /** Called when the user selects a ref; should replace the token in `value`. */
  onSelect: (next: string, nextCaret: number) => void;
  /** Called when the popup closes (selection or escape). */
  onClose: () => void;
}

/**
 * Floating ref-suggestion popup, rendered into a portal and pinned to the
 * caret position of `element`.
 */
export default function RefAwarePopup({
  element,
  value,
  caret,
  refContext,
  onSelect,
  onClose,
}: RefAwarePopupProps) {
  const document = useDocumentStore((s) => s.document);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);
  const measure = useCaretPosition();
  const [activeIndex, setActiveIndex] = useState(0);

  const token = useMemo(() => detectToken(value, caret), [value, caret]);
  const allRefs = useMemo(
    () => computeRefs(document, refContext, activeWorkflowName ?? undefined),
    [document, refContext, activeWorkflowName],
  );

  const filtered = useMemo<RefSuggestion[]>(() => {
    if (!token) return [];
    const q = token.query;
    if (!q) return allRefs;
    const scored = allRefs
      .map((r) => ({ r, score: fuzzyScore(q, r.label) }))
      .filter((s): s is { r: RefSuggestion; score: number } => s.score !== null)
      .sort((a, b) => b.score - a.score);
    return scored.map((s) => s.r);
  }, [allRefs, token]);

  // Reset the active index whenever the filtered list changes.
  useEffect(() => {
    setActiveIndex(0);
  }, [filtered.length, token?.query]);

  const apply = (suggestion: RefSuggestion) => {
    if (!token) return;
    // Replace `{{<query>` (no closing `}}` yet) with the full ref.
    const before = value.slice(0, token.start);
    const after = value.slice(token.caret);
    const next = before + suggestion.value + after;
    const nextCaret = before.length + suggestion.value.length;
    onSelect(next, nextCaret);
  };

  // Keyboard handling
  useEffect(() => {
    if (!element || !token) return;
    const handler = (e: KeyboardEvent) => {
      if (filtered.length === 0) {
        if (e.key === "Escape") {
          e.preventDefault();
          onClose();
        }
        return;
      }
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setActiveIndex((i) => (i + 1) % filtered.length);
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setActiveIndex((i) => (i - 1 + filtered.length) % filtered.length);
      } else if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        const choice = filtered[activeIndex];
        if (choice) apply(choice);
      } else if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    };
    element.addEventListener("keydown", handler as EventListener);
    return () => element.removeEventListener("keydown", handler as EventListener);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [element, token, filtered, activeIndex, onClose]);

  if (!element || !token || filtered.length === 0) return null;

  const caretPos = measure(element, token.start);
  const grouped = groupRefs(filtered);

  return createPortal(
    <div
      role="listbox"
      className="fixed z-50 max-h-[280px] w-[260px] overflow-y-auto rounded-md border border-border-default bg-surface-1 shadow-xl text-xs animate-fade-in"
      style={{
        top: caretPos.top + caretPos.height + 4,
        left: caretPos.left,
      }}
    >
      {(() => {
        let flatIndex = 0;
        const blocks: React.ReactNode[] = [];
        for (const [group, items] of grouped) {
          blocks.push(
            <div
              key={`h-${group}`}
              className="sticky top-0 bg-surface-1 px-2 py-1 text-[10px] uppercase tracking-wider text-fg-subtle border-b border-border-default"
            >
              {GROUP_LABELS[group] ?? group}
            </div>,
          );
          for (const item of items) {
            const localIndex = flatIndex;
            const isActive = localIndex === activeIndex;
            blocks.push(
              <button
                key={item.value}
                type="button"
                role="option"
                aria-selected={isActive}
                className={`flex w-full items-center justify-between gap-2 px-2 py-1 text-left ${
                  isActive
                    ? "bg-accent-soft text-fg-default"
                    : "text-fg-muted hover:bg-surface-2"
                }`}
                onMouseDown={(e) => {
                  e.preventDefault();
                  apply(item);
                }}
                onMouseEnter={() => setActiveIndex(localIndex)}
              >
                <span className="truncate font-mono">{item.label}</span>
                {item.detail && (
                  <span className="shrink-0 text-[10px] text-fg-subtle font-mono">
                    {item.detail}
                  </span>
                )}
              </button>,
            );
            flatIndex += 1;
          }
        }
        return blocks;
      })()}
    </div>,
    window.document.body,
  );
}

export { detectToken };
