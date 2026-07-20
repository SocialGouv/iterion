import { describe, expect, it } from "vitest";

import {
  childrenClosedCount,
  childrenProgressPct,
  formatChildrenClosedRatio,
  formatChildrenSummary,
} from "./planGroups";

const summary = {
  total: 5,
  ready: 2,
  in_progress: 1,
  done: 1,
  failed: 1,
  open: 1,
};

describe("children counters", () => {
  // "Closed" on this board means finished OR failed — a failed child is
  // done with, not still pending, so it counts toward the campaign total.
  it("counts done + failed as closed", () => {
    expect(childrenClosedCount(summary)).toBe(2);
    expect(childrenProgressPct(summary)).toBe(40);
  });

  it("leads with closed/total", () => {
    expect(formatChildrenClosedRatio(summary)).toBe("2/5 closed");
    expect(formatChildrenSummary(summary)).toMatch(/^2\/5 closed/);
    expect(formatChildrenSummary(summary)).toContain("1 live");
  });

  it("degrades safely with no summary", () => {
    expect(childrenClosedCount(undefined)).toBe(0);
    expect(childrenProgressPct(undefined)).toBe(0);
    expect(formatChildrenClosedRatio(undefined)).toBe("");
    expect(formatChildrenClosedRatio(undefined, 3)).toBe("0/3 closed");
    expect(formatChildrenSummary(undefined, 3)).toBe("3 children");
  });
});
