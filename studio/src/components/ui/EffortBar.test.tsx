// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { EffortBar } from "./EffortBar";

afterEach(cleanup);

function filledCells(container: HTMLElement): number {
  return container.querySelectorAll("span.bg-fg-muted\\/60, span.bg-fg-muted, span.bg-accent, span.bg-warning, span.bg-danger\\/70, span.bg-danger").length;
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
  // carries it (pi, the codex fallback) — and there the normalised
  // index+1 math must not paint a cell for the no-reasoning level.
  it("draws an empty bar for none even when the model's supported list carries it", () => {
    const { container } = render(
      <EffortBar level="none" supported={["none", "low", "medium", "high"]} />,
    );
    expect(filledCells(container)).toBe(0);
  });

  it("still normalises other levels to the supported range", () => {
    const { container } = render(
      <EffortBar level="high" supported={["none", "low", "medium", "high"]} />,
    );
    expect(filledCells(container)).toBe(4);
  });
});
