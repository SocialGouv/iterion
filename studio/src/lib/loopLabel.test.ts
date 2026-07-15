import { describe, expect, it } from "vitest";

import { formatLoopBadge } from "./loopLabel";

describe("formatLoopBadge", () => {
  it("renders a literal bound as loop:name(N)", () => {
    expect(formatLoopBadge({ name: "refine_loop", max_iterations: 5 })).toBe(
      "loop:refine_loop(5)",
    );
  });

  it("omits the parens when the bound is absent (unbounded form)", () => {
    // jsonenc.go marshals max_iterations with omitempty, so the
    // unbounded form arrives with no bound field at all.
    expect(formatLoopBadge({ name: "continuation_loop" })).toBe(
      "loop:continuation_loop",
    );
  });

  it("treats a zero bound as absent", () => {
    expect(formatLoopBadge({ name: "l", max_iterations: 0 })).toBe("loop:l");
  });

  it("renders the expression bound for the template form", () => {
    expect(
      formatLoopBadge({ name: "fix_loop", max_iterations_expr: "{{vars.max}}" }),
    ).toBe("loop:fix_loop({{vars.max}})");
  });

  it("truncates long expression bounds", () => {
    const expr = "{{outputs.select_candidate.fix_loop_max}}";
    const out = formatLoopBadge({ name: "fix_loop", max_iterations_expr: expr });
    expect(out).toBe(`loop:fix_loop(${expr.slice(0, 24)}…)`);
  });

  it("renders the explicit unbounded form, with and without fuel", () => {
    expect(formatLoopBadge({ name: "l", unbounded: true })).toBe(
      "loop:l(unbounded)",
    );
    expect(formatLoopBadge({ name: "l", unbounded: true, fuel_cap: 40 })).toBe(
      "loop:l(unbounded 40)",
    );
  });

  it("never renders the string 'undefined'", () => {
    expect(formatLoopBadge({ name: "l" })).not.toContain("undefined");
  });
});
