// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { CostChart } from "./index";

afterEach(cleanup);

// RGAA 1.6 (#2503): the chart's figures lived in hover-only <title> —
// the sr-only data table exposes them as real text.
describe("CostChart data table", () => {
  it("exposes the per-day per-workflow figures as text", () => {
    render(
      <CostChart
        buckets={[
          { day: "2026-10-25", cost_by_workflow: { alpha: 0.5, beta: 0.25 }, total: 0.75 },
          { day: "2026-10-26", cost_by_workflow: { alpha: 1.5 }, total: 1.5 },
        ]}
        workflows={["alpha", "beta"]}
        colors={{}}
      />,
    );
    const table = screen.getByRole("table", { name: /cost per day/i });
    expect(table).toBeTruthy();
    expect(screen.getByText("2026-10-25")).toBeTruthy();
    expect(screen.getByText("2026-10-26")).toBeTruthy();
    expect(screen.getAllByRole("columnheader").length).toBe(4);
    expect(screen.getAllByRole("row").length).toBeGreaterThanOrEqual(3);
  });
});
