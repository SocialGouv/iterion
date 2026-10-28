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

  // The blur restores the WHOLE inline style it snapshot, so these two assert
  // the property the clamp owns and then the bytes: a mirror maintained
  // property by property is what let a `display` and a `white-space` survive
  // a blur, each breaking the block's layout from then on.
  it("unclamps on focus and restores the clamp verbatim on blur", () => {
    const { container } = render(<ClampText text={TEXT} className="block" />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    const before = clamped.style.cssText;
    expect(clamped.style.overflow).toBe("hidden");
    expect(clamped.style.whiteSpace).toBe("nowrap");
    expect(clamped.style.display).toBe("");
    fireEvent.focus(clamped);
    expect(clamped.style.overflow).toBe("visible");
    expect(clamped.style.whiteSpace).toBe("normal");
    expect(clamped.style.display).toBe("block");
    fireEvent.blur(clamped);
    expect(clamped.style.cssText).toBe(before);
  });

  it("leaves a multi-line clamp exactly as it found it, white-space included", () => {
    const { container } = render(<ClampText text={TEXT} lines={2} />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    const before = clamped.style.cssText;
    expect(clamped.style.display).toBe("-webkit-box");
    expect(clamped.style.whiteSpace).toBe("");
    expect(clamped.style.webkitLineClamp).toBe("2");
    fireEvent.focus(clamped);
    expect(clamped.style.display).toBe("block");
    // The clamp itself is lifted, not just its display mode: a residual
    // -webkit-line-clamp keeps the text at two lines in a browser that maps
    // it onto the standard property, and the reveal shows nothing.
    expect(clamped.style.webkitLineClamp).toBe("");
    expect(clamped.style.webkitBoxOrient).toBe("");
    // Untouched while focused: a multi-line clamp wraps already, and writing
    // `normal` over an inherited `pre-wrap` would collapse the newlines of
    // the text the focus just revealed.
    expect(clamped.style.whiteSpace).toBe("");
    fireEvent.blur(clamped);
    expect(clamped.style.cssText).toBe(before);
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
