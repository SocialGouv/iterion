import { describe, expect, it } from "vitest";

import { prettyMd } from "./NodeOutputCard";

describe("prettyMd", () => {
  it("skips blank string fields instead of rendering a bare label", () => {
    // A gate node's empty fail_log on success must not leave a
    // "FAIL LOG" heading with nothing under it.
    const md = prettyMd({ converged: true, fail_log: "" });
    expect(md).toContain("Converged");
    expect(md).toContain("true");
    expect(md).not.toContain("Fail log");
  });

  it("skips whitespace-only string fields too", () => {
    const md = prettyMd({ summary: "all good", notes: "   \n  " });
    expect(md).toContain("Summary");
    expect(md).not.toContain("Notes");
  });

  it("keeps non-blank fields and explicit non-string empties", () => {
    const md = prettyMd({ verdict: "pass", findings: [], score: 0 });
    expect(md).toContain("Verdict");
    // Empty array renders an explicit "(empty)" marker — informative,
    // unlike a blank string.
    expect(md).toContain("Findings");
    expect(md).toContain("_(empty)_");
    expect(md).toContain("Score");
  });

  it("renders a single string field verbatim", () => {
    expect(prettyMd({ answer: "just the prose" })).toBe("just the prose");
  });

  it("returns empty markdown when every field is blank", () => {
    expect(prettyMd({ a: "", b: "  " })).toBe("");
  });
});
