import { describe, it, expect } from "vitest";
import type { AgentDecl, IterDocument, WorkflowDecl } from "@/api/types";
import { computeRefs } from "./refCompletion";

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

function agent(name: string, input: string, output: string): AgentDecl {
  return { name, model: "m", input, output, system: "s", user: "u", session: "fresh" } as AgentDecl;
}

describe("computeRefs — edge-with {{input.*}}", () => {
  const d = doc({
    schemas: [
      { name: "src_out", fields: [{ name: "produced", type: "string" }] },
      { name: "dst_in", fields: [{ name: "from_dst", type: "string" }] },
    ],
    agents: [agent("src", "dst_in", "src_out"), agent("dst", "dst_in", "src_out")],
    workflows: [
      {
        name: "w",
        entry: "src",
        edges: [{ from: "src", to: "dst" }],
      } as WorkflowDecl,
    ],
  });

  it("suggests the source node's output fields, not the destination's input", () => {
    const refs = computeRefs(d, { kind: "edge-with", edgeFrom: "src", edgeTo: "dst" });
    const inputVals = refs.filter((r) => r.group === "input").map((r) => r.value);
    expect(inputVals).toContain("{{input.produced}}");
    expect(inputVals).not.toContain("{{input.from_dst}}");
  });

  it("still uses the consuming node's input schema for a prompt", () => {
    const refs = computeRefs(d, { kind: "node-prompt", nodeId: "dst" });
    const inputVals = refs.filter((r) => r.group === "input").map((r) => r.value);
    expect(inputVals).toContain("{{input.from_dst}}");
    expect(inputVals).not.toContain("{{input.produced}}");
  });

  it("suggests a router's incoming with-keys, not the destination input schema", () => {
    const routed = doc({
      schemas: [{ name: "dst_in", fields: [{ name: "from_dst", type: "string" }] }],
      routers: [{ name: "dispatch", mode: "fan_out_all" }],
      agents: [agent("src", "", "src_out"), agent("dst", "dst_in", "src_out")],
      workflows: [
        {
          name: "w",
          entry: "src",
          edges: [
            { from: "src", to: "dispatch", with: [{ key: "topic", value: "{{outputs.src.topic}}" }] },
            { from: "dispatch", to: "dst" },
          ],
        } as WorkflowDecl,
      ],
    });
    const refs = computeRefs(routed, { kind: "edge-with", edgeFrom: "dispatch", edgeTo: "dst" });
    const inputVals = refs.filter((r) => r.group === "input").map((r) => r.value);
    expect(inputVals).toContain("{{input.topic}}");
    expect(inputVals).not.toContain("{{input.from_dst}}");
  });
});
