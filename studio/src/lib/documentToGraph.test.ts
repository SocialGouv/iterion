import { describe, it, expect } from "vitest";
import type { IterDocument, WorkflowDecl } from "@/api/types";
import {
  documentHasEditableNodes,
  documentToGraph,
  getTopologyKey,
} from "./documentToGraph";

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

// The canvas draws its "No workflow loaded" overlay from this predicate, so a
// kind missing here HIDES a real workflow behind an empty state. That is not
// hypothetical: `computes` was absent, and a deterministic (LLM-free)
// workflow — every node a compute — rendered as if nothing had loaded.
describe("documentHasEditableNodes", () => {
  it("is false for a document with no nodes at all", () => {
    expect(documentHasEditableNodes(doc({}))).toBe(false);
  });

  it("counts a compute-only workflow as loaded", () => {
    const d = doc({
      computes: [{ name: "say_hello", output: "hello_out", expr: [] }],
    });
    expect(documentHasEditableNodes(d)).toBe(true);
  });

  it("counts every kind the canvas can draw", () => {
    // Minimal stubs: this predicate only counts, it never reads a field.
    const cases = [
      { agents: [{ name: "a" }] },
      { judges: [{ name: "j" }] },
      { routers: [{ name: "r", mode: "condition" }] },
      { humans: [{ name: "h" }] },
      { tools: [{ name: "t", command: "", output: "o" }] },
      { computes: [{ name: "c", output: "o" }] },
      { subbots: [{ name: "s", source: "x.bot", isolated: true }] },
    ] as unknown as Array<Partial<IterDocument>>;
    for (const partial of cases) {
      expect(documentHasEditableNodes(doc(partial))).toBe(true);
    }
  });
});

// RGAA 10.3 (#2521): with stylesheets disabled the canvas reads in DOM
// order, which is the node array's insertion order — the virtual __start__
// node must come FIRST, not after the terminal nodes, or the reading order
// runs the workflow backwards.
describe("documentToGraph — the __start__ node order", () => {
  const d = doc({
    subbots: [{ name: "produce_episode", source: "episode.bot", isolated: true }],
    workflows: [
      {
        name: "main",
        entry: "plan",
        edges: [
          { from: "plan", to: "produce_episode" },
          { from: "produce_episode", to: "done" },
          { from: "produce_episode", to: "fail" },
        ],
      } as WorkflowDecl,
    ],
    tools: [{ name: "plan", command: "", output: "o" }],
  });

  it("emits __start__ before every parsed node and both terminals", () => {
    const names = documentToGraph(d, "main").nodes.map((n) => n.id);
    expect(names[0]).toBe("__start__");
    expect(names.indexOf("__start__")).toBeLessThan(names.indexOf("done"));
    expect(names.indexOf("__start__")).toBeLessThan(names.indexOf("fail"));
  });

  it("still emits the referenced done/fail terminals", () => {
    // The first landing of this fix dropped the fail fill with the moved
    // block, and the expanded subbot lost its ::fail child.
    const names = documentToGraph(d, "main").nodes.map((n) => n.id);
    expect(names).toContain("done");
    expect(names).toContain("fail");
  });

  it("drops __start__ when the entry names no declared node", () => {
    const broken = doc({
      tools: [{ name: "plan", command: "", output: "o" }],
      workflows: [{ name: "main", entry: "ghost", edges: [] } as WorkflowDecl],
    });
    const { nodes, edges } = documentToGraph(broken, "main");
    expect(nodes.some((n) => n.id === "__start__")).toBe(false);
    expect(edges.some((e) => e.source === "__start__")).toBe(false);
  });

  it("keeps the start edge aimed at the entry node", () => {
    const { edges } = documentToGraph(d, "main");
    expect(edges.some((e) => e.source === "__start__" && e.target === "plan")).toBe(true);
  });
});
