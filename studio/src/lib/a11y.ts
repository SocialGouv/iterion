import type { KeyboardEvent, MouseEvent } from "react";

/**
 * What activated a row. `pointer` carries the cursor position of the click
 * that activated it, and is null when a key did — a call site that needs
 * coordinates reads them from here instead of writing its own `onClick`
 * beside the spread, which would SHADOW the helper's (a later JSX prop wins)
 * and silently drop whatever the helper's handler also does.
 */
export interface RowActivation {
  pointer: { x: number; y: number } | null;
}

const pointerOf = (e: MouseEvent<HTMLElement>): RowActivation => ({
  pointer: { x: e.clientX, y: e.clientY },
});

// A fresh object per activation, never a shared one: a call site that reads
// it keeps whatever it stores, and nothing it does reaches the next row.
const keyActivation = (): RowActivation => ({ pointer: null });

// Props that promote a non-button element (a table row, list item, or card)
// to a keyboard-operable button: click + Enter/Space activation, plus the
// role/tabindex a screen reader needs. Spread onto the element and keep
// its own className/key/title. Replaces the ~5-line Enter/Space onKeyDown
// idiom that had drifted across a dozen clickable-row/card call-sites.
//
// ONLY for a row with NO focusable descendant. A role="button" that holds a
// control is a button containing a button: invalid ARIA, reported by axe as
// `nested-interactive` (WCAG 4.1.2), and the inner control's Enter also
// activates the row. Such a row takes `pointerRowProps` and hands its
// keyboard path to a real control inside it. Neither half is a review
// convention: the source guard `_clickableRows.ts` holds both.
//
// `label` is optional on purpose (RGAA 7.1): an aria-label OVERRIDES the
// element's content as the accessible name, so a row with visible text
// must NOT carry one — its name is its visible content, which contains
// the visible label by construction. Only a row with no usable visible
// text passes a label.
export function clickableRowProps(
  onActivate: (activation: RowActivation) => void,
  label?: string,
  opts?: { disabled?: boolean },
) {
  const { disabled } = opts ?? {};
  return {
    role: "button" as const,
    // A disabled control leaves the tab order: nothing on it activates,
    // and focusable-but-inert is a keyboard trap (RGAA 7.1).
    tabIndex: disabled ? -1 : 0,
    ...(label ? { "aria-label": label } : {}),
    onClick: disabled ? undefined : (e: MouseEvent<HTMLElement>) => onActivate(pointerOf(e)),
    onKeyDown: (e: KeyboardEvent<HTMLElement>) => {
      if (disabled) return;
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        onActivate(keyActivation());
      }
    },
  };
}

/**
 * Props for a row whose click is a POINTER CONVENIENCE — the row reacts to a
 * click, and a real control INSIDE it carries the same action for the
 * keyboard. No role, no tabIndex, no key handler, on purpose: this is the
 * shape for a row that holds a focusable descendant, where `role="button"`
 * would be `nested-interactive` (WCAG 4.1.2) and would put the row AND each
 * nested control in the tab order for one row's worth of actions.
 *
 * The inner control is what makes the pointer-only row legal (RGAA 7.3): a
 * row with nothing focusable inside is mouse-only, so it takes
 * `clickableRowProps` instead. `_clickableRows.ts` refuses both mistakes.
 */
export function pointerRowProps(
  onActivate: (activation: RowActivation) => void,
  opts?: { disabled?: boolean },
) {
  const { disabled } = opts ?? {};
  return {
    onClick: disabled ? undefined : (e: MouseEvent<HTMLElement>) => onActivate(pointerOf(e)),
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
