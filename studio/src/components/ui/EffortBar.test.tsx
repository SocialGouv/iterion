// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { EffortBar } from "./EffortBar";

afterEach(cleanup);

function filledCells(container: HTMLElement): number {
  return container.querySelectorAll("span.bg-fg-muted\\/60, span.bg-fg-muted, span.bg-accent, span.bg-warning, span.bg-danger\\/70, span.bg-danger").length;
}

function totalCells(container: HTMLElement): number {
  return container.querySelectorAll("span.w-\\[3px\\]").length;
}

describe("EffortBar", () => {
  it("draws an empty bar for none on the global scale", () => {
    const { container } = render(<EffortBar level="none" />);
    expect(filledCells(container)).toBe(0);
  });

  it("draws one cell for low on the global scale", () => {
    const { container } = render(<EffortBar level="low" />);
    expect(filledCells(container)).toBe(1);
  });

  // none is a real level exactly on the backends whose supported list
  // carries it (pi, codex/claw on GPT-6 Sol/Luna) — and there the
  // normalised index+1 math must not paint a cell for the no-reasoning
  // level, nor give it a slot of its own.
  it("draws an empty bar for none even when the model's supported list carries it", () => {
    const { container } = render(
      <EffortBar level="none" supported={["none", "low", "medium", "high"]} />,
    );
    expect(filledCells(container)).toBe(0);
    expect(totalCells(container)).toBe(3);
  });

  it("still normalises other levels to the supported range", () => {
    const { container } = render(
      <EffortBar level="high" supported={["none", "low", "medium", "high"]} />,
    );
    expect(filledCells(container)).toBe(3);
    expect(totalCells(container)).toBe(3);
  });

  // The lowest real level reads the same with or without none in the list:
  // a none slot would shift every level up a cell (pi's low drew 2/6).
  it("fills one cell for the lowest real level whether or not the model carries none", () => {
    const withNone = render(
      <EffortBar level="low" supported={["none", "low", "medium", "high", "xhigh", "max"]} />,
    );
    expect(filledCells(withNone.container)).toBe(1);
    expect(totalCells(withNone.container)).toBe(5);
    cleanup();

    const withoutNone = render(
      <EffortBar level="low" supported={["low", "medium", "high", "xhigh", "max"]} />,
    );
    expect(filledCells(withoutNone.container)).toBe(1);
    expect(totalCells(withoutNone.container)).toBe(5);
  });
});
