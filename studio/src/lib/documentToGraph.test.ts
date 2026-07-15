import { describe, it, expect } from "vitest";
import type { IterDocument, WorkflowDecl } from "@/api/types";
import { documentToGraph, getTopologyKey } from "./documentToGraph";

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

describe("documentToGraph — subbot declarations", () => {
  const d = doc({
    tools: [{ name: "plan", command: "", output: "o" }],
    subbots: [{ name: "produce_episode", source: "episode.bot", isolated: true }],
    workflows: [
      {
        name: "main",
        entry: "plan",
        edges: [
          { from: "plan", to: "produce_episode" },
          { from: "produce_episode", to: "done" },
        ],
      } as WorkflowDecl,
    ],
  });

  it("renders a subbot as a compact workflowNode of kind subbot", () => {
    const { nodes } = documentToGraph(d, "main");
    const sb = nodes.find((n) => n.id === "produce_episode")!;
    expect(sb).toBeDefined();
    expect(sb.type).toBe("workflowNode");
    expect(sb.data.kind).toBe("subbot");
    expect(sb.data.color).toBe("var(--color-node-subbot)");
    expect((sb.data.decl as { source?: string }).source).toBe("episode.bot");
  });

  it("resolves edges referencing the subbot node", () => {
    const { edges } = documentToGraph(d, "main");
    expect(edges.some((e) => e.source === "plan" && e.target === "produce_episode")).toBe(true);
    expect(edges.some((e) => e.source === "produce_episode" && e.target === "done")).toBe(true);
  });

  it("includes subbots in the topology key counts", () => {
    const without = doc({ tools: [{ name: "plan", command: "", output: "o" }] });
    const withSubbot = doc({
      tools: [{ name: "plan", command: "", output: "o" }],
      subbots: [{ name: "sb", source: "x.bot" }],
    });
    expect(getTopologyKey(without)).not.toBe(getTopologyKey(withSubbot));
  });
});
