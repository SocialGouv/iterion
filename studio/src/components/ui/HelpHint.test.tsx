// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { HelpHint } from "./HelpHint";

// The "?" glyph is an image with a text alternative, not a generic span:
// ARIA in HTML prohibits aria-label on a roleless span (axe
// aria-prohibited-attr) and a screen reader would otherwise hear a bare
// "?" with no hint.

afterEach(cleanup);

describe("HelpHint", () => {
  it("exposes its hint text as the name of a non-generic role", () => {
    render(<HelpHint text="Timeout applies per node" />);
    const hint = screen.getByRole("img", { name: "Timeout applies per node" });
    expect(hint.getAttribute("title")).toBe("Timeout applies per node");
  });

  it("HelpHint — labelled glyph, axe smoke", async () => {
    const { container } = render(<HelpHint text="Runs only on the main branch" />);
    await expectNoViolationsStrict(container, "HelpHint");
  });
});
