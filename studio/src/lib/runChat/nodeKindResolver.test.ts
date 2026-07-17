import { describe, expect, it } from "vitest";

import { humanizeNodeId } from "./nodeKindResolver";

describe("humanizeNodeId", () => {
  it("spaces separators and capitalizes the first word", () => {
    expect(humanizeNodeId("campaign_feature")).toBe("Campaign feature");
    expect(humanizeNodeId("verify-run")).toBe("Verify run");
  });

  it("drops a leading stage token", () => {
    expect(humanizeNodeId("s2_review_dep_deps_scan")).toBe(
      "Review dep deps scan",
    );
    expect(humanizeNodeId("p1_plan")).toBe("Plan");
    expect(humanizeNodeId("step3_ship_it")).toBe("Ship it");
  });

  it("keeps a stage-like token when it is the whole id", () => {
    expect(humanizeNodeId("s2")).toBe("S2");
  });

  it("passes through ids without separators", () => {
    expect(humanizeNodeId("triage")).toBe("Triage");
  });
});
