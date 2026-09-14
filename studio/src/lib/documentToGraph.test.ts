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

describe("documentToGraph — native public graph", () => {
  const d = doc({
    contracts: [
      { name: "Root", display_name: "Deliver", responsibility: "Ship outputs", inputs: [{ name: "seed", type: "string" }], outputs: [{ name: "report", type: "string" }, { name: "result", type: "string" }] },
      { name: "Produce", display_name: "Produce", responsibility: "Produce a value", inputs: [{ name: "seed", type: "string" }], outputs: [{ name: "value", type: "string" }] },
      { name: "Join", display_name: "Join both", responsibility: "Combine values", inputs: [{ name: "left", type: "string" }, { name: "right", type: "string" }], outputs: [{ name: "value", type: "string" }] },
    ],
    computes: [
      { name: "producer_impl", output: "", expr: [] },
      { name: "join_impl", output: "", expr: [] },
      { name: "unused_impl", output: "", expr: [] },
    ],
    workflows: [{
      name: "deliver", runtime_semantics: "ports-v1", contract: "Root", entry: "", edges: [],
      graph: {
        nodes: [
          { name: "a", implementation: "producer_impl", contract: "Produce" },
          { name: "b", implementation: "producer_impl", contract: "Produce" },
          { name: "c", implementation: "join_impl", contract: "Join" },
        ],
        bindings: [
          { from: "input.seed", to: "a.seed" }, { from: "input.seed", to: "b.seed" },
          { from: "a.value", to: "c.left" }, { from: "b.value", to: "c.right" },
        ],
        exports: [{ name: "report", from: "a.value" }, { name: "result", from: "c.value" }],
        products: ["report"],
      },
    }],
  });

  it("shows public instances, multi-input join and independently exported product", () => {
    const { nodes, edges } = documentToGraph(d, "deliver");
    expect(nodes.map(node => node.id)).toEqual(["__inputs__", "a", "b", "c", "__outputs__"]);
    expect(nodes.find(node => node.id === "c")?.data.label).toBe("Join both");
    expect(edges.filter(edge => edge.target === "c").map(edge => edge.source)).toEqual(["a", "b"]);
    expect(edges.some(edge => edge.source === "a" && edge.target === "c")).toBe(true);
    expect(edges.some(edge => edge.source === "a" && edge.target === "__outputs__" && String(edge.label).includes("product"))).toBe(true);
  });

  it("keys layout to bindings and instances rather than unused technical declarations", () => {
    const key = getTopologyKey(d, "deliver");
    expect(getTopologyKey(doc({ ...d, computes: [...d.computes, { name: "more_unused", output: "", expr: [] }] }), "deliver")).toBe(key);
    const changed = doc({ ...d, workflows: [{ ...d.workflows[0]!, graph: { ...d.workflows[0]!.graph,
      bindings: [...d.workflows[0]!.graph!.bindings!, { from: "b.value", to: "c.extra" }] } }] });
    expect(getTopologyKey(changed, "deliver")).not.toBe(key);
  });

  it("labels array-to-scalar expansion and distinguishes whole-array passing", () => {
    const mapped = doc({ ...d, contracts: [
      ...d.contracts!,
      { name: "Batch", display_name: "Batch", responsibility: "Supply items", inputs: [], outputs: [{ name: "items", type: "string[]" }] },
      { name: "Collect", display_name: "Collect", responsibility: "Consume all items", inputs: [{ name: "items", type: "string[]" }], outputs: [] },
    ], workflows: [{ ...d.workflows[0]!, graph: {
      nodes: [
        { name: "batch", implementation: "producer_impl", contract: "Batch" },
        { name: "item", implementation: "producer_impl", contract: "Produce" },
        { name: "collect", implementation: "join_impl", contract: "Collect" },
      ],
      bindings: [{ from: "batch.items", to: "item.seed" }, { from: "batch.items", to: "collect.items" }],
      exports: [],
    } }] });
    const { edges } = documentToGraph(mapped, "deliver");
    expect(String(edges.find(edge => edge.target === "item")?.label)).toContain("map each (dynamic)");
    expect(String(edges.find(edge => edge.target === "collect")?.label)).not.toContain("map each");
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
