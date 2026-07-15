import { describe, it, expect } from "vitest";
import type { Node, Edge as FlowEdge } from "@xyflow/react";
import type { IterDocument, WorkflowDecl } from "@/api/types";
import { documentToGraph } from "./documentToGraph";
import type { NodeData } from "./documentToGraph";
import { autoLayout } from "./autoLayout";
import {
  expandSubbots,
  getSubbotExpansionKey,
  isSubbotChildId,
  makeSubbotChildId,
  parseSubbotChildId,
  resolveSubbotSource,
  type SubbotChildDoc,
} from "./subbotGraph";

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

// Parent doc mirroring examples/pipeline-board-demo/main.bot:
// plan -> dispatch(fan_out_each) -> produce_episode(subbot) -> collect -> done
function parentDoc(): IterDocument {
  return doc({
    tools: [{ name: "plan", command: "", output: "plan_out" }],
    routers: [{ name: "dispatch", mode: "fan_out_each", over: "{{outputs.plan.episodes}}", as: "ep" }],
    subbots: [
      {
        name: "produce_episode",
        source: "episode.bot",
        with: [{ key: "episode", value: "{{outputs.dispatch.ep.id}}" }],
        output: "episode_out",
        isolated: true,
      },
    ],
    computes: [{ name: "collect", await: "wait_all", output: "collect_out", expr: [] }],
    workflows: [
      {
        name: "pipeline_board_demo",
        entry: "plan",
        edges: [
          { from: "plan", to: "dispatch" },
          { from: "dispatch", to: "produce_episode" },
          { from: "produce_episode", to: "collect" },
          { from: "collect", to: "done" },
        ],
      } as WorkflowDecl,
    ],
  });
}

// Child doc mirroring examples/pipeline-board-demo/episode.bot:
// produce -> review -> (approved) wrap -> done / (not approved) fail
function childDoc(): IterDocument {
  return doc({
    tools: [{ name: "produce", command: "", output: "produce_out" }],
    humans: [{ name: "review", input: "", output: "review_out", instructions: "" }],
    computes: [{ name: "wrap", output: "episode_out", expr: [] }],
    workflows: [
      {
        name: "episode",
        entry: "produce",
        edges: [
          { from: "produce", to: "review" },
          { from: "review", to: "wrap", when: { condition: "approved" } },
          { from: "review", to: "fail", when: { condition: "approved", negated: true } },
          { from: "wrap", to: "done" },
        ],
      } as WorkflowDecl,
    ],
  });
}

function loadedChild(overrides?: Partial<SubbotChildDoc>): Map<string, SubbotChildDoc> {
  return new Map([
    ["produce_episode", { path: "examples/pipeline-board-demo/episode.bot", doc: childDoc(), ...overrides }],
  ]);
}

function expand(childDocs: Map<string, SubbotChildDoc>, parent = parentDoc()) {
  const base = documentToGraph(parent, parent.workflows[0]!.name) as {
    nodes: Node<NodeData>[];
    edges: FlowEdge[];
  };
  return expandSubbots(base, parent, childDocs);
}

describe("id helpers", () => {
  it("round-trips a child id", () => {
    const id = makeSubbotChildId("produce_episode", "produce");
    expect(id).toBe("produce_episode::produce");
    expect(isSubbotChildId(id)).toBe(true);
    expect(parseSubbotChildId(id)).toEqual({ subbotId: "produce_episode", childId: "produce" });
  });

  it("does not flag plain node ids", () => {
    expect(isSubbotChildId("produce_episode")).toBe(false);
    expect(parseSubbotChildId("dispatch")).toBeNull();
  });
});

describe("resolveSubbotSource", () => {
  it("joins relative to the parent file's directory", () => {
    expect(resolveSubbotSource("examples/pipeline-board-demo/main.bot", "episode.bot")).toBe(
      "examples/pipeline-board-demo/episode.bot",
    );
  });

  it("normalizes . and ..", () => {
    expect(resolveSubbotSource("a/b/main.bot", "../c/./child.bot")).toBe("a/c/child.bot");
  });

  it("falls back to the bare source without a parent path", () => {
    expect(resolveSubbotSource(null, "episode.bot")).toBe("episode.bot");
  });

  it("never escapes above the workspace root", () => {
    expect(resolveSubbotSource("main.bot", "../../child.bot")).toBe("child.bot");
  });
});

describe("expandSubbots", () => {
  it("replaces the compact node with a subbotFrame container", () => {
    const { nodes } = expand(loadedChild());
    const frame = nodes.find((n) => n.id === "produce_episode")!;
    expect(frame.type).toBe("subbotFrame");
    expect(frame.data.kind).toBe("subbot");
    expect(frame.data.source).toBe("episode.bot");
    expect(frame.data.isolated).toBe(true);
    expect(frame.data.childWorkflowName).toBe("episode");
    expect(frame.data.sourcePath).toBe("examples/pipeline-board-demo/episode.bot");
    // Exactly one node carries the subbot's id (no duplicate compact node)
    expect(nodes.filter((n) => n.id === "produce_episode")).toHaveLength(1);
  });

  it("prefixes child node ids, parents them into the frame, marks them external", () => {
    const { nodes } = expand(loadedChild());
    const childIds = nodes.filter((n) => isSubbotChildId(n.id)).map((n) => n.id).sort();
    expect(childIds).toEqual([
      "produce_episode::done",
      "produce_episode::fail",
      "produce_episode::produce",
      "produce_episode::review",
      "produce_episode::wrap",
    ]);
    for (const id of childIds) {
      const n = nodes.find((x) => x.id === id)!;
      expect(n.parentId).toBe("produce_episode");
      expect(n.extent).toBe("parent");
      expect(n.data.external).toBe(true);
      expect(n.data.subbotId).toBe("produce_episode");
      expect(n.data.subbotSource).toBe("episode.bot");
    }
  });

  it("places the frame before its children in the node array (React Flow parent order)", () => {
    const { nodes } = expand(loadedChild());
    const frameIdx = nodes.findIndex((n) => n.id === "produce_episode");
    for (const n of nodes) {
      if (isSubbotChildId(n.id)) {
        expect(nodes.indexOf(n)).toBeGreaterThan(frameIdx);
      }
    }
  });

  it("drops the child's virtual __start__ node and its entry edge", () => {
    const { nodes, edges } = expand(loadedChild());
    expect(nodes.some((n) => isSubbotChildId(n.id) && n.id.endsWith("__start__"))).toBe(false);
    expect(edges.some((e) => isSubbotChildId(e.source) && e.source.endsWith("__start__"))).toBe(false);
    // The parent's own __start__ survives
    expect(nodes.some((n) => n.id === "__start__")).toBe(true);
  });

  it("retargets the parent edge INTO the subbot to the child entry", () => {
    const { edges } = expand(loadedChild());
    const inEdge = edges.find((e) => e.source === "dispatch")!;
    expect(inEdge.target).toBe("produce_episode::produce");
  });

  it("re-sources the parent edge OUT of the subbot from the child's done node", () => {
    const { edges } = expand(loadedChild());
    const outEdge = edges.find((e) => e.target === "collect")!;
    expect(outEdge.source).toBe("produce_episode::done");
  });

  it("keeps the frame as edge source when the child graph has no done node", () => {
    const child = childDoc();
    // Reroute the child terminal edge to fail only, so `done` is never referenced
    child.workflows[0]!.edges = [
      { from: "produce", to: "review" },
      { from: "review", to: "fail" },
    ];
    const { edges } = expand(new Map([["produce_episode", { path: "p/episode.bot", doc: child }]]));
    const outEdge = edges.find((e) => e.target === "collect")!;
    expect(outEdge.source).toBe("produce_episode");
  });

  it("rewires BOTH endpoints of a self-loop edge on the subbot", () => {
    const parent = parentDoc();
    parent.workflows[0]!.edges = [
      { from: "plan", to: "dispatch" },
      { from: "dispatch", to: "produce_episode" },
      {
        from: "produce_episode",
        to: "produce_episode",
        loop: { name: "retry", max_iterations: 3 },
      },
      { from: "produce_episode", to: "collect" },
      { from: "collect", to: "done" },
    ];
    const { edges } = expand(loadedChild(), parent);
    const selfLoop = edges.find((e) => e.data?.loop)!;
    expect(selfLoop.source).toBe("produce_episode::done");
    expect(selfLoop.target).toBe("produce_episode::produce");
  });

  it("preserves condition labels and data on internal child edges", () => {
    const { edges } = expand(loadedChild());
    const approved = edges.find(
      (e) => e.source === "produce_episode::review" && e.target === "produce_episode::wrap",
    )!;
    expect(approved.label).toBe("approved");
    const rejected = edges.find(
      (e) => e.source === "produce_episode::review" && e.target === "produce_episode::fail",
    )!;
    expect(rejected.label).toBe("!approved");
  });

  it("preserves when/loop annotations on rewired parent edges", () => {
    const parent = parentDoc();
    parent.workflows[0]!.edges = [
      { from: "plan", to: "dispatch" },
      { from: "dispatch", to: "produce_episode", when: { condition: "ready" } },
      { from: "produce_episode", to: "collect", loop: { name: "retry", max_iterations: 5 } },
      { from: "collect", to: "done" },
    ];
    const { edges } = expand(loadedChild(), parent);
    const inEdge = edges.find((e) => e.target === "produce_episode::produce")!;
    expect(inEdge.label).toBe("ready");
    expect((inEdge.data as { when?: { condition?: string } }).when?.condition).toBe("ready");
    const outEdge = edges.find((e) => e.target === "collect")!;
    expect(outEdge.source).toBe("produce_episode::done");
    expect((outEdge.data as { loop?: { name: string } }).loop?.name).toBe("retry");
  });

  it("keeps every edge id unique", () => {
    const { edges } = expand(loadedChild());
    const ids = edges.map((e) => e.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("keeps the compact node with loadError when the child failed to load", () => {
    const { nodes, edges } = expand(
      new Map([["produce_episode", { path: "p/episode.bot", error: "404 not found" }]]),
    );
    const compact = nodes.find((n) => n.id === "produce_episode")!;
    expect(compact.type).toBe("workflowNode");
    expect(compact.data.loadError).toBe("404 not found");
    // Edges untouched: still point at the compact node
    expect(edges.some((e) => e.source === "dispatch" && e.target === "produce_episode")).toBe(true);
    expect(edges.some((e) => e.source === "produce_episode" && e.target === "collect")).toBe(true);
  });

  it("keeps the compact node while the child doc is still loading (no entry)", () => {
    const { nodes } = expand(new Map());
    const compact = nodes.find((n) => n.id === "produce_episode")!;
    expect(compact.type).toBe("workflowNode");
    expect(compact.data.kind).toBe("subbot");
  });

  it("expands one level only: a subbot inside the child renders compact", () => {
    const child = childDoc();
    child.subbots = [{ name: "inner", source: "inner.bot" }];
    child.workflows[0]!.edges = [
      { from: "produce", to: "inner" },
      { from: "inner", to: "done" },
    ];
    const { nodes } = expand(new Map([["produce_episode", { path: "p/episode.bot", doc: child }]]));
    const inner = nodes.find((n) => n.id === "produce_episode::inner")!;
    expect(inner.type).toBe("workflowNode"); // compact, not a nested frame
    expect(inner.data.kind).toBe("subbot");
    expect(inner.data.external).toBe(true);
  });

  it("expands multiple subbot nodes independently", () => {
    const parent = parentDoc();
    parent.subbots = [
      ...(parent.subbots ?? []),
      { name: "publish_episode", source: "publish.bot" },
    ];
    parent.workflows[0]!.edges = [
      { from: "plan", to: "dispatch" },
      { from: "dispatch", to: "produce_episode" },
      { from: "produce_episode", to: "publish_episode" },
      { from: "publish_episode", to: "collect" },
      { from: "collect", to: "done" },
    ];
    const publishChild = doc({
      tools: [{ name: "upload", command: "", output: "out" }],
      workflows: [
        { name: "publish", entry: "upload", edges: [{ from: "upload", to: "done" }] } as WorkflowDecl,
      ],
    });
    const childDocs = new Map<string, SubbotChildDoc>([
      ["produce_episode", { path: "p/episode.bot", doc: childDoc() }],
      ["publish_episode", { path: "p/publish.bot", doc: publishChild }],
    ]);
    const { nodes, edges } = expand(childDocs, parent);
    expect(nodes.find((n) => n.id === "produce_episode")!.type).toBe("subbotFrame");
    expect(nodes.find((n) => n.id === "publish_episode")!.type).toBe("subbotFrame");
    // frame-to-frame link: produce's done -> publish's entry
    const between = edges.find((e) => e.source === "produce_episode::done")!;
    expect(between.target).toBe("publish_episode::upload");
    const out = edges.find((e) => e.target === "collect")!;
    expect(out.source).toBe("publish_episode::done");
    const ids = edges.map((e) => e.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("is a no-op for documents without subbots", () => {
    const plain = doc({
      tools: [{ name: "t", command: "", output: "o" }],
      workflows: [{ name: "w", entry: "t", edges: [{ from: "t", to: "done" }] } as WorkflowDecl],
    });
    const base = documentToGraph(plain, "w") as { nodes: Node<NodeData>[]; edges: FlowEdge[] };
    const result = expandSubbots(base, plain, new Map());
    expect(result).toBe(base);
  });
});

describe("autoLayout of an expanded subbot graph", () => {
  // Guards the ELK INCLUDE_CHILDREN contract this feature leans on:
  // cross-hierarchy edges (dispatch -> frame::produce, frame::done ->
  // collect) declared at the root must lay out without error, the frame
  // must be sized as a compound, and children must sit inside it.
  it("lays out the compound frame with children inside and sane cross-hierarchy edges", async () => {
    const { nodes, edges } = expand(loadedChild());
    const laid = await autoLayout(nodes as Node[], edges, "DOWN");

    const frame = laid.find((n) => n.id === "produce_episode")!;
    const style = frame.style as { width?: number; height?: number };
    expect(style.width).toBeGreaterThan(100);
    expect(style.height).toBeGreaterThan(100);

    for (const n of laid) {
      if (!isSubbotChildId(n.id)) continue;
      // Child positions are parent-relative: inside the frame bounds.
      expect(n.position.x).toBeGreaterThanOrEqual(0);
      expect(n.position.y).toBeGreaterThanOrEqual(0);
      expect(n.position.x).toBeLessThanOrEqual(style.width!);
      expect(n.position.y).toBeLessThanOrEqual(style.height!);
    }
  });
});

describe("getSubbotExpansionKey", () => {
  it("changes when a child doc finishes loading", () => {
    const parent = parentDoc();
    const pending = getSubbotExpansionKey(parent, new Map());
    const loaded = getSubbotExpansionKey(parent, loadedChild());
    expect(pending).not.toBe(loaded);
  });

  it("changes when the child topology changes", () => {
    const parent = parentDoc();
    const a = getSubbotExpansionKey(parent, loadedChild());
    const modified = childDoc();
    modified.workflows[0]!.edges = [{ from: "produce", to: "done" }];
    const b = getSubbotExpansionKey(
      parent,
      new Map([["produce_episode", { path: "examples/pipeline-board-demo/episode.bot", doc: modified }]]),
    );
    expect(a).not.toBe(b);
  });

  it("is empty for documents without subbots", () => {
    expect(getSubbotExpansionKey(doc({}), new Map())).toBe("");
    expect(getSubbotExpansionKey(null, new Map())).toBe("");
  });
});
