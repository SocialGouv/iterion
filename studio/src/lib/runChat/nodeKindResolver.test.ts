import { describe, expect, it } from "vitest";

import type { WireWorkflow } from "@/api/runs";

import { humanizeNodeId, irKindResolver } from "./nodeKindResolver";

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

describe("irKindResolver label", () => {
  const workflow: WireWorkflow = {
    name: "wf",
    entry: "campaign_feature",
    nodes: [
      {
        id: "campaign_feature",
        kind: "agent",
        description: "Campaign: implement & commit units",
      },
      { id: "verify_run", kind: "tool" },
      { id: "blank_desc", kind: "agent", description: "   " },
    ],
    edges: [],
  };

  it("prefers the authored description over the humanized id", () => {
    const r = irKindResolver(workflow);
    expect(r.label("campaign_feature")).toBe(
      "Campaign: implement & commit units",
    );
  });

  it("falls back to humanizeNodeId when no description is set", () => {
    const r = irKindResolver(workflow);
    expect(r.label("verify_run")).toBe("Verify run");
  });

  it("treats a blank description as absent", () => {
    const r = irKindResolver(workflow);
    expect(r.label("blank_desc")).toBe("Blank desc");
  });

  it("falls back for unknown node ids and a null workflow", () => {
    expect(irKindResolver(workflow).label("not_in_graph")).toBe("Not in graph");
    expect(irKindResolver(null).label("campaign_feature")).toBe(
      "Campaign feature",
    );
  });
});
