// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { ClampText } from "./ClampText";
import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

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

  // One announcement channel: clipping is visual, so the block's own text IS
  // its whole accessible content. An sr-only copy of the same string, and an
  // aria-describedby pointed at it, announce the text two and three times
  // (#2790) — both are absent, and they move together: a describedby whose
  // target is gone is an axe aria-valid-attr-value violation of its own.
  it("announces the clamped text exactly once — as its own content", () => {
    const { container } = render(<ClampText text={TEXT} className="block" />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    // The exact string, not a `contains`: a second copy anywhere inside the
    // block doubles this and reddens.
    expect(clamped.textContent).toBe(TEXT);
    expect(clamped.querySelector(".sr-only")).toBeNull();
    expect(clamped.getAttribute("aria-describedby")).toBeNull();
  });

  // Inside a control the block is not a second tab stop for one control: no
  // tabindex of its own, and no reveal handlers to fire — the control is the
  // one stop, and its accessible name carries the text (#2803). The node
  // stays marked data-clamp-text: still the primitive's render.
  it("renders non-focusable on demand, the control around it being the stop", () => {
    const { container } = render(<ClampText text={TEXT} focusable={false} />);
    const clamped = container.querySelector("[data-clamp-text]") as HTMLElement;
    expect(clamped).not.toBeNull();
    expect(clamped.getAttribute("tabindex")).toBeNull();
    const before = clamped.style.cssText;
    fireEvent.focus(clamped);
    fireEvent.blur(clamped);
    expect(clamped.style.cssText).toBe(before);
  });

  // The composition the nested case is about: a clamp in a <button> audits
  // with NO waiver — the node is not focusable, so focus-order-semantics has
  // nothing to waive. A ClampText that made tabIndex unconditional again
  // reddens this entry, as does a call site that drops the opt-out (the
  // source guard in jsx-content.test.ts).
  it("sits in a button with no violation and no waiver — the button's name carries the text", async () => {
    const { container } = render(
      <button type="button">
        <ClampText text="Search or run…" focusable={false} />
      </button>,
    );
    // The text is the button's accessible name, as content — not a duplicate
    // description pinned on top of it.
    expect(screen.getByRole("button", { name: /Search or run/ })).toBeTruthy();
    await expectNoViolationsStrict(container, "button with a nested ClampText");
  });
});
