import { describe, expect, it } from "vitest";

import type { ExecutionState, RunSummary, WireWorkflow } from "@/api/runs";
import { expandWireSubbots, mergeChildExecutions } from "./subbotRunGraph";

// Mirrors examples/pipeline-board-demo: dispatch fans out over the
// (single) subbot node produce_episode, collect joins.
function parentWf(overrides?: Partial<WireWorkflow>): WireWorkflow {
  return {
    name: "pipeline_board_demo",
    entry: "plan",
    nodes: [
      { id: "plan", kind: "tool" },
      { id: "dispatch", kind: "router" },
      { id: "produce_episode", kind: "subbot", source: "episode.bot", isolated: true },
      { id: "collect", kind: "compute" },
      { id: "done", kind: "done" },
    ],
    edges: [
      { from: "plan", to: "dispatch" },
      { from: "dispatch", to: "produce_episode" },
      { from: "produce_episode", to: "collect" },
      { from: "collect", to: "done" },
    ],
    ...overrides,
  };
}

// Mirrors episode.bot: produce -> review -> wrap -> done / fail.
function childWf(overrides?: Partial<WireWorkflow>): WireWorkflow {
  return {
    name: "episode",
    entry: "produce",
    nodes: [
      { id: "produce", kind: "tool" },
      { id: "review", kind: "human" },
      { id: "wrap", kind: "compute" },
      { id: "done", kind: "done" },
      { id: "fail", kind: "fail" },
    ],
    edges: [
      { from: "produce", to: "review" },
      { from: "review", to: "wrap", condition: "approved" },
      { from: "review", to: "fail", condition: "approved", negated: true },
      { from: "wrap", to: "done" },
    ],
    ...overrides,
  };
}

const CHILD_BY_NODE = new Map([["produce_episode", childWf()]]);

function exec(partial: Partial<ExecutionState>): ExecutionState {
  return {
    execution_id: "exec:main:produce:0",
    ir_node_id: "produce",
    branch_id: "main",
    loop_iteration: 0,
    status: "running",
    current_event_seq: 3,
    first_seq: 2,
    last_seq: 3,
    ...partial,
  };
}

function child(partial: Partial<RunSummary>): RunSummary {
  return {
    id: "child-1",
    workflow_name: "episode",
    status: "running",
    created_at: "2026-07-15T10:00:00Z",
    updated_at: "2026-07-15T10:00:00Z",
    active: true,
    parent_run_id: "parent-1",
    parent_node_id: "produce_episode",
    ...partial,
  };
}

describe("expandWireSubbots", () => {
  it("is a no-op when no subbot node has a child workflow", () => {
    const wf = parentWf();
    const out = expandWireSubbots(wf, new Map());
    expect(out.frames).toEqual([]);
    expect(out.wf).toBe(wf);
  });

  it("replaces the subbot node with the child pipeline's prefixed nodes", () => {
    const { wf, frames } = expandWireSubbots(parentWf(), CHILD_BY_NODE);
    expect(wf.nodes.some((n) => n.id === "produce_episode")).toBe(false);
    const childIds = wf.nodes
      .filter((n) => n.parentSubbot === "produce_episode")
      .map((n) => n.id);
    expect(childIds).toEqual([
      "produce_episode::produce",
      "produce_episode::review",
      "produce_episode::wrap",
      "produce_episode::done",
      "produce_episode::fail",
    ]);
    expect(frames).toEqual([
      {
        id: "produce_episode",
        source: "episode.bot",
        isolated: true,
        childWorkflowName: "episode",
      },
    ]);
  });

  it("rewires the parent edges end-to-end (into entry, out of done)", () => {
    const { wf } = expandWireSubbots(parentWf(), CHILD_BY_NODE);
    expect(
      wf.edges.some(
        (e) => e.from === "dispatch" && e.to === "produce_episode::produce",
      ),
    ).toBe(true);
    expect(
      wf.edges.some(
        (e) => e.from === "produce_episode::done" && e.to === "collect",
      ),
    ).toBe(true);
    // Internal child edges are prefixed with conditions intact.
    const rejected = wf.edges.find(
      (e) =>
        e.from === "produce_episode::review" &&
        e.to === "produce_episode::fail",
    )!;
    expect(rejected.condition).toBe("approved");
    expect(rejected.negated).toBe(true);
  });

  it("rewires BOTH endpoints of a self-loop edge on the subbot", () => {
    const wf = parentWf();
    wf.edges.push({
      from: "produce_episode",
      to: "produce_episode",
      loop: "retry",
    });
    const out = expandWireSubbots(wf, CHILD_BY_NODE);
    const selfLoop = out.wf.edges.find((e) => e.loop === "retry")!;
    expect(selfLoop.from).toBe("produce_episode::done");
    expect(selfLoop.to).toBe("produce_episode::produce");
  });

  it("keeps the frame as edge source when the child has no done node", () => {
    const noDone = childWf({
      nodes: [
        { id: "produce", kind: "tool" },
        { id: "fail", kind: "fail" },
      ],
      edges: [{ from: "produce", to: "fail" }],
    });
    const { wf } = expandWireSubbots(
      parentWf(),
      new Map([["produce_episode", noDone]]),
    );
    expect(
      wf.edges.some((e) => e.from === "produce_episode" && e.to === "collect"),
    ).toBe(true);
  });

  it("expands NESTED subbots recursively (frames within frames)", () => {
    // Mirrors examples/nested-subbots-demo: main -> stage(subbot) whose
    // workflow fans out over step(subbot).
    const rootWf: WireWorkflow = {
      name: "nested_subbots_demo",
      entry: "prep",
      nodes: [
        { id: "prep", kind: "tool" },
        { id: "stage", kind: "subbot", source: "stage.bot" },
        { id: "wrap", kind: "compute" },
        { id: "done", kind: "done" },
      ],
      edges: [
        { from: "prep", to: "stage" },
        { from: "stage", to: "wrap" },
        { from: "wrap", to: "done" },
      ],
    };
    const stageWf: WireWorkflow = {
      name: "stage",
      entry: "split",
      nodes: [
        { id: "split", kind: "tool" },
        { id: "dispatch", kind: "router" },
        { id: "step", kind: "subbot", source: "step.bot", isolated: true },
        { id: "gather", kind: "compute" },
        { id: "done", kind: "done" },
      ],
      edges: [
        { from: "split", to: "dispatch" },
        { from: "dispatch", to: "step" },
        { from: "step", to: "gather" },
        { from: "gather", to: "done" },
      ],
    };
    const stepWf: WireWorkflow = {
      name: "step",
      entry: "work",
      nodes: [
        { id: "work", kind: "tool" },
        { id: "check", kind: "human" },
        { id: "out", kind: "compute" },
        { id: "done", kind: "done" },
      ],
      edges: [
        { from: "work", to: "check" },
        { from: "check", to: "out", condition: "approved" },
        { from: "out", to: "done" },
      ],
    };
    const { wf, frames } = expandWireSubbots(
      rootWf,
      new Map([
        ["stage", stageWf],
        // Nested key = the EXPANDED id of stage's step node.
        ["stage::step", stepWf],
      ]),
    );
    // Two frames, inner one parented into the outer.
    expect(frames.map((f) => ({ id: f.id, parent: f.parentSubbot }))).toEqual([
      { id: "stage", parent: undefined },
      { id: "stage::step", parent: "stage" },
    ]);
    // Grandchild nodes carry the chained ids + immediate frame parent.
    const work = wf.nodes.find((n) => n.id === "stage::step::work")!;
    expect(work.parentSubbot).toBe("stage::step");
    // Both subbot nodes are gone (replaced by frames).
    expect(wf.nodes.some((n) => n.id === "stage" || n.id === "stage::step")).toBe(false);
    // Edges rewired at BOTH levels: dispatch -> step's entry, step's
    // done -> gather, prep -> stage's entry, stage's done -> wrap.
    expect(wf.edges.some((e) => e.from === "stage::dispatch" && e.to === "stage::step::work")).toBe(true);
    expect(wf.edges.some((e) => e.from === "stage::step::done" && e.to === "stage::gather")).toBe(true);
    expect(wf.edges.some((e) => e.from === "prep" && e.to === "stage::split")).toBe(true);
    expect(wf.edges.some((e) => e.from === "stage::done" && e.to === "wrap")).toBe(true);
  });

  it("expands only the subbot nodes whose child workflow is known", () => {
    const wf = parentWf({
      nodes: [
        ...parentWf().nodes,
        { id: "publish", kind: "subbot", source: "publish.bot" },
      ],
      edges: [...parentWf().edges, { from: "collect", to: "publish" }],
    });
    const out = expandWireSubbots(wf, CHILD_BY_NODE);
    expect(out.frames.map((f) => f.id)).toEqual(["produce_episode"]);
    expect(out.wf.nodes.some((n) => n.id === "publish")).toBe(true);
  });
});

describe("mergeChildExecutions", () => {
  const byNode = new Map([
    ["produce_episode", [child({ id: "c1" }), child({ id: "c2" })]],
  ]);
  const execsByRun = new Map([
    ["c1", [exec({ first_seq: 2, last_seq: 3 })]],
    ["c2", [exec({ status: "paused_waiting_human", first_seq: 5, last_seq: 9 })]],
  ]);

  it("projects ONLY the selected child's executions onto the frame's node ids", () => {
    const merged = mergeChildExecutions(
      byNode,
      execsByRun,
      new Map([["produce_episode", "c2"]]),
    );
    expect(merged).toHaveLength(1);
    expect(merged[0]!.ir_node_id).toBe("produce_episode::produce");
    expect(merged[0]!.execution_id).toBe("c2::exec:main:produce:0");
    expect(merged[0]!.status).toBe("paused_waiting_human");
  });

  it("switches content when the selected child changes", () => {
    const forC1 = mergeChildExecutions(
      byNode,
      execsByRun,
      new Map([["produce_episode", "c1"]]),
    );
    expect(forC1[0]!.execution_id).toBe("c1::exec:main:produce:0");
    expect(forC1[0]!.status).toBe("running");
  });

  it("neutralizes branch ids so child internals never join the parent's fan-out regions", () => {
    const merged = mergeChildExecutions(
      byNode,
      execsByRun,
      new Map([["produce_episode", "c1"]]),
    );
    expect(merged.every((ex) => ex.branch_id.startsWith("subrun::"))).toBe(true);
  });

  it("skips frames without a selection, unknown selections, and children without executions", () => {
    expect(mergeChildExecutions(byNode, execsByRun, new Map())).toEqual([]);
    expect(
      mergeChildExecutions(
        byNode,
        execsByRun,
        new Map([["produce_episode", "ghost"]]),
      ),
    ).toEqual([]);
    expect(
      mergeChildExecutions(
        byNode,
        new Map(),
        new Map([["produce_episode", "c1"]]),
      ),
    ).toEqual([]);
  });
});
