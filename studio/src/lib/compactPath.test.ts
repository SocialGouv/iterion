import { describe, it, expect } from "vitest";

import { compactPath } from "./compactPath";

describe("compactPath", () => {
  it("keeps short paths untouched", () => {
    expect(compactPath("main.bot")).toBe("main.bot");
    expect(compactPath("/home/jo/main.bot")).toBe("/home/jo/main.bot");
  });

  it("compacts long absolute paths to the last two segments", () => {
    expect(
      compactPath("/tmp/claude-1000/-home-jo-lab/95ebd1bc/scratchpad/probe.bot"),
    ).toBe("…/scratchpad/probe.bot");
    expect(compactPath("/home/jo/lab/ai/iterion/bots/docs-refresh/main.bot")).toBe(
      "…/docs-refresh/main.bot",
    );
  });

  it("honours a custom keep count", () => {
    expect(compactPath("/a/b/c/d/e.bot", 3)).toBe("…/c/d/e.bot");
  });
});
