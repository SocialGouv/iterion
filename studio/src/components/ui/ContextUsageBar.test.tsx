// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { ContextUsageBar } from "./ContextUsageBar";

afterEach(cleanup);

// A gauge is announced as a gauge: role="img" would have hidden the value
// assistive tech can otherwise report and compare (#2591's surface triage).
describe("ContextUsageBar", () => {
  it("reports its value as a progressbar", () => {
    render(<ContextUsageBar used={45_000} window={100_000} />);
    const bar = screen.getByRole("progressbar");
    expect(bar.getAttribute("aria-valuemin")).toBe("0");
    expect(bar.getAttribute("aria-valuemax")).toBe("100000");
    expect(bar.getAttribute("aria-valuenow")).toBe("45000");
    expect(bar.getAttribute("aria-valuetext")).toBe(
      "context: 45,000 / 100,000 tokens (45%)",
    );
    // The name says WHICH gauge; the same sentence in both is read twice,
    // around the role.
    expect(bar.getAttribute("aria-label")).toBe("Context usage");
    expect(bar.getAttribute("aria-label")).not.toBe(
      bar.getAttribute("aria-valuetext"),
    );
  });

  it("stays in range when the proxy reports more usage than window", () => {
    // formatContextUsage clamps the percentage for exactly this case, so the
    // gauge must clamp with it: a valuenow above valuemax is out of range,
    // and assistive tech that recomputes the percentage would announce
    // >100%. The exact figures stay in valuetext.
    render(<ContextUsageBar used={120_000} window={100_000} />);
    const bar = screen.getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("100000");
    expect(bar.getAttribute("aria-valuetext")).toBe(
      "context: 120,000 / 100,000 tokens (100%)",
    );
  });

  it("stays quiet when the window was never reported", () => {
    const { container } = render(<ContextUsageBar used={10} window={undefined} />);
    expect(container.firstChild).toBeNull();
  });

  it("ui · ContextUsageBar — axe smoke, incompletes included", async () => {
    const { container } = render(
      <main>
        <ContextUsageBar used={92_000} window={100_000} />
      </main>,
    );
    await expectNoViolationsStrict(container, "ContextUsageBar");
  });
});
