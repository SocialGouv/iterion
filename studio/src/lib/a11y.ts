import type { KeyboardEvent } from "react";

// Props that promote a non-button element (a table row, list item, or card)
// to a keyboard-operable button: click + Enter/Space activation, plus the
// role/tabindex/label a screen reader needs. Spread onto the element and keep
// its own className/key/title. Replaces the ~5-line Enter/Space onKeyDown
// idiom that had drifted across a dozen clickable-row/card call-sites.
export function clickableRowProps(onActivate: () => void, label: string) {
  return {
    role: "button" as const,
    tabIndex: 0,
    "aria-label": label,
    onClick: onActivate,
    onKeyDown: (e: KeyboardEvent<HTMLElement>) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        onActivate();
      }
    },
  };
}

// Everything a Tab press can land on. Read in DOM order on purpose: the app
// uses the natural order everywhere, and a positive tabindex would be a bug of
// its own rather than something to honour here.
const FOCUSABLE_SELECTOR = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  "summary",
  "[tabindex]",
].join(",");

export function focusableWithin(root: HTMLElement): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
    (el) =>
      el.tabIndex >= 0 &&
      !el.hasAttribute("hidden") &&
      el.getAttribute("aria-hidden") !== "true",
  );
}

// trapTabKey keeps Tab / Shift+Tab cycling inside `root`. It is the missing
// half of an `aria-modal="true"` dialog that is NOT built on Radix (which
// brings its own trap): without it, Tab walks the page behind the scrim while
// a screen reader is told the rest of the page is inert.
//
// Wire it as the container's onKeyDown, and give the container `tabIndex={-1}`
// so focus can rest on it before the first Tab. Returns true when it handled
// (and preventDefault-ed) the event.
export function trapTabKey(
  e: KeyboardEvent<HTMLElement>,
  root: HTMLElement | null,
): boolean {
  if (e.key !== "Tab" || !root) return false;
  const items = focusableWithin(root);
  const first = items[0];
  const last = items[items.length - 1];
  if (!first || !last) {
    // Nothing to focus but the container itself — still don't let Tab escape.
    e.preventDefault();
    root.focus();
    return true;
  }
  const active = document.activeElement;
  // Focus resting on the container itself counts as "outside the ring": the
  // next Tab enters at the correct end rather than jumping to the browser's
  // idea of the next element.
  const inRing = active instanceof HTMLElement && items.includes(active);
  if (e.shiftKey) {
    if (!inRing || active === first) {
      e.preventDefault();
      last.focus();
      return true;
    }
    return false;
  }
  if (!inRing || active === last) {
    e.preventDefault();
    first.focus();
    return true;
  }
  return false;
}
