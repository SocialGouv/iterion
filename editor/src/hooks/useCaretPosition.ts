import { useCallback, useRef } from "react";

export interface CaretPosition {
  /** Caret coordinates in viewport space (matches getBoundingClientRect.left/top). */
  top: number;
  left: number;
  height: number;
}

const MIRRORED_PROPS = [
  "boxSizing",
  "width",
  "height",
  "overflowX",
  "overflowY",
  "borderTopWidth",
  "borderRightWidth",
  "borderBottomWidth",
  "borderLeftWidth",
  "borderStyle",
  "paddingTop",
  "paddingRight",
  "paddingBottom",
  "paddingLeft",
  "fontStyle",
  "fontVariant",
  "fontWeight",
  "fontStretch",
  "fontSize",
  "fontSizeAdjust",
  "lineHeight",
  "fontFamily",
  "textAlign",
  "textTransform",
  "textIndent",
  "textDecoration",
  "letterSpacing",
  "wordSpacing",
  "tabSize",
  "MozTabSize",
  "whiteSpace",
  "wordWrap",
  "wordBreak",
] as const;

/**
 * Returns a `measure` function that computes the caret position for the
 * given input or textarea.
 *
 * Works by rendering an invisible div that mirrors the element's typography
 * and inserting the text up to the caret. The trailing span's bounding rect
 * gives the caret coordinates.
 */
export function useCaretPosition() {
  const mirrorRef = useRef<HTMLDivElement | null>(null);

  const ensureMirror = useCallback(() => {
    if (mirrorRef.current && mirrorRef.current.isConnected) return mirrorRef.current;
    const el = document.createElement("div");
    el.setAttribute("aria-hidden", "true");
    Object.assign(el.style, {
      position: "absolute",
      visibility: "hidden",
      top: "0",
      left: "0",
      pointerEvents: "none",
      whiteSpace: "pre-wrap",
      wordWrap: "break-word",
    } as CSSStyleDeclaration);
    document.body.appendChild(el);
    mirrorRef.current = el;
    return el;
  }, []);

  const measure = useCallback(
    (
      el: HTMLInputElement | HTMLTextAreaElement,
      caretIndex: number,
    ): CaretPosition => {
      const mirror = ensureMirror();
      const computed = window.getComputedStyle(el);
      for (const prop of MIRRORED_PROPS) {
        // CSSStyleDeclaration is indexable via these strings.
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        (mirror.style as any)[prop] = (computed as any)[prop];
      }
      // Inputs are single-line; force the mirror to behave like one.
      if (el.tagName === "INPUT") {
        mirror.style.whiteSpace = "pre";
        mirror.style.wordWrap = "normal";
      }
      const text = el.value.slice(0, caretIndex);
      mirror.textContent = text;
      const cursor = document.createElement("span");
      cursor.textContent = "\u200b"; // zero-width space anchor
      mirror.appendChild(cursor);

      const rect = el.getBoundingClientRect();
      const cursorRect = cursor.getBoundingClientRect();
      const mirrorRect = mirror.getBoundingClientRect();

      const lineHeight =
        parseFloat(computed.lineHeight) || parseFloat(computed.fontSize) * 1.2;

      // Translate the mirror coordinates into the input element's space.
      const offsetX = cursorRect.left - mirrorRect.left;
      const offsetY = cursorRect.top - mirrorRect.top;

      // Inputs scroll horizontally; account for that.
      const scrollLeft = el.scrollLeft;
      const scrollTop = "scrollTop" in el ? (el as HTMLTextAreaElement).scrollTop : 0;

      return {
        left: rect.left + offsetX - scrollLeft,
        top: rect.top + offsetY - scrollTop,
        height: lineHeight,
      };
    },
    [ensureMirror],
  );

  return measure;
}
