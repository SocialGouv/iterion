import { describe, it, expect } from "vitest";
import { computeFanoutRegions, buildFanoutFrames } from "./fanoutRegions";
import type { IterDocument, WorkflowDecl } from "@/api/types";

// Minimal document/workflow builders — only the fields computeFanoutRegions
// reads (router mode, node await, edges).
function doc(partial: Partial<IterDocument>): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [],
    comments: [],
    ...partial,
  } as IterDocument;
}

describe("computeFanoutRegions", () => {
  it("returns the per-item region between a fan_out_each router and its wait_all join", () => {
    const d = doc({
      routers: [{ name: "dispatch", mode: "fan_out_each", over: "{{vars.items}}" }],
      tools: [
        { name: "start", command: "", output: "ok" },
        { name: "work", command: "", output: "ok" },
        { name: "collect", command: "", output: "ok", await: "wait_all" },
      ],
    });
    const wf = {
      name: "demo",
      entry: "start",
      edges: [
        { from: "start", to: "dispatch" },
        { from: "dispatch", to: "work" },
        { from: "work", to: "collect" },
        { from: "collect", to: "done" },
      ],
    } as unknown as WorkflowDecl;

    const regions = computeFanoutRegions(d, wf);
    expect(regions).toHaveLength(1);
    expect(regions[0]!.router).toBe("dispatch");
    // Only `work` runs per-item; collect (wait_all) is the boundary, start is upstream.
    expect([...regions[0]!.nodeIds].sort()).toEqual(["work"]);
  });

  it("captures a multi-node branch body (mark_ip + route + per-type stubs)", () => {
    const d = doc({
      routers: [
        { name: "dispatch", mode: "fan_out_each", over: "{{vars.t}}" },
        { name: "route", mode: "condition" },
      ],
      tools: [
        { name: "mark_ip", command: "", output: "ok" },
        { name: "code_stub", command: "", output: "ok" },
        { name: "design_stub", command: "", output: "ok" },
        { name: "collect", command: "", output: "ok", await: "wait_all" },
      ],
    });
    const wf = {
      name: "demo",
      entry: "mark_ip",
      edges: [
        { from: "dispatch", to: "mark_ip" },
        { from: "mark_ip", to: "route" },
        { from: "route", to: "code_stub" },
        { from: "route", to: "design_stub" },
        { from: "code_stub", to: "collect" },
        { from: "design_stub", to: "collect" },
        { from: "collect", to: "done" },
      ],
    } as unknown as WorkflowDecl;

    const regions = computeFanoutRegions(d, wf);
    expect(regions).toHaveLength(1);
    expect([...regions[0]!.nodeIds].sort()).toEqual([
      "code_stub",
      "design_stub",
      "mark_ip",
      "route",
    ]);
  });

  it("returns no regions when there is no fan_out_each router", () => {
    const d = doc({
      routers: [{ name: "r", mode: "fan_out_all" }],
      tools: [{ name: "a", command: "", output: "ok" }],
    });
    const wf = { name: "demo", entry: "a", edges: [{ from: "r", to: "a" }] } as unknown as WorkflowDecl;
    expect(computeFanoutRegions(d, wf)).toHaveLength(0);
  });

  it("builds one frame node sized to the region bbox", () => {
    const regions = [{ router: "dispatch", nodeIds: new Set(["work"]) }];
    const layoutNodes = [
      { id: "work", position: { x: 100, y: 200 }, data: {} },
    ] as never[];
    const frames = buildFanoutFrames(layoutNodes, regions);
    expect(frames).toHaveLength(1);
    expect(frames[0]!.id).toBe("__fanout__dispatch");
    expect(frames[0]!.type).toBe("fanoutFrame");
    // position is bbox.min minus padding/header → strictly above-left of the node
    expect(frames[0]!.position.x).toBeLessThan(100);
    expect(frames[0]!.position.y).toBeLessThan(200);
  });
});
