// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { ClampText } from "./ClampText";

afterEach(cleanup);

const TEXT = "a very long description";

describe("ClampText", () => {
  // What a surface's axe waiver for focus-order-semantics names: this focus
  // stop is the recovery mechanism (RGAA 10.12), not an accident.
  it("puts the clamped block in the focus order", () => {
    const { container } = render(<ClampText text={TEXT} className="block" />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    expect(clamped).not.toBeNull();
    expect(clamped.tabIndex).toBe(0);
    expect(clamped.getAttribute("role")).toBeNull();
  });

  it("unclamps on focus and re-clamps on blur, display included", () => {
    const { container } = render(<ClampText text={TEXT} className="block" />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    expect(clamped.style.overflow).toBe("hidden");
    const displayBefore = clamped.style.display;
    fireEvent.focus(clamped);
    expect(clamped.style.overflow).toBe("visible");
    expect(clamped.style.whiteSpace).toBe("normal");
    expect(clamped.style.display).toBe("block");
    fireEvent.blur(clamped);
    expect(clamped.style.overflow).toBe("hidden");
    expect(clamped.style.whiteSpace).toBe("nowrap");
    // A single-line clamp owns no display of its own, so the focused `block`
    // must not survive the blur: one Tab past an inline clamp would break its
    // line for the rest of the page's life.
    expect(clamped.style.display).toBe(displayBefore);
  });

  it("keeps a multi-line clamp a -webkit-box after blur, not a nowrap line", () => {
    const { container } = render(<ClampText text={TEXT} lines={2} />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    fireEvent.focus(clamped);
    expect(clamped.style.display).toBe("block");
    fireEvent.blur(clamped);
    expect(clamped.style.display).toBe("-webkit-box");
    expect(clamped.style.whiteSpace).toBe("normal");
  });

  // The half that makes the focus stop safe rather than a trap: an assistive
  // technology gets the whole text without focusing anything, so the roleless
  // stop costs a sighted keyboard user a Tab and costs a screen-reader user
  // nothing. Asserted on the BLOCK's own text, which is the property; the
  // extra sr-only copy the component also renders, and points its
  // aria-describedby at, announces the string a second and third time
  // (#2790) — pinned here only as "the reference resolves", never as the
  // shape to keep.
  it("keeps the whole text reachable without focusing anything", () => {
    const { container } = render(<ClampText text={TEXT} className="block" />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    expect(clamped.textContent).toContain(TEXT);
    const describedBy = clamped.getAttribute("aria-describedby") ?? "";
    expect(describedBy).not.toBe("");
    // A dangling aria-describedby is an axe violation of its own, so the
    // target has to exist whatever the duplication is resolved to.
    expect(container.querySelector(`#${CSS.escape(describedBy)}`)).not.toBeNull();
    expect(screen.getAllByText(TEXT).length).toBeGreaterThanOrEqual(1);
  });
});
