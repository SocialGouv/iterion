import { describe, expect, it } from "vitest";

import type { RunStatus, RunSummary, WireWorkflow } from "@/api/runs";
import {
  childTabLabel,
  groupChildrenByNode,
  isSettledRunStatus,
  statusDotClass,
} from "./subRuns";

// The unattributed bucket key groupChildrenByNode uses internally.
const UNATTRIBUTED_NODE = "";

function child(partial: Partial<RunSummary>): RunSummary {
  return {
    id: "child-1",
    workflow_name: "episode",
    status: "running",
    created_at: "2026-07-15T10:00:00Z",
    updated_at: "2026-07-15T10:00:00Z",
    active: true,
    parent_run_id: "parent-1",
    ...partial,
  };
}

// Mirrors examples/pipeline-board-demo/main.bot: dispatch fans out over
// episodes, produce_episode is the (single) subbot node, collect joins.
function demoWf(overrides?: Partial<WireWorkflow>): WireWorkflow {
  return {
    name: "pipeline-board-demo",
    entry: "plan",
    nodes: [
      { id: "plan", kind: "agent" },
      { id: "dispatch", kind: "router" },
      { id: "produce_episode", kind: "subbot", source: "episode.bot", isolated: true },
      { id: "collect", kind: "agent" },
      { id: "final_review", kind: "human" },
      { id: "finish", kind: "compute" },
      { id: "done", kind: "done" },
    ],
    edges: [
      { from: "plan", to: "dispatch" },
      { from: "dispatch", to: "produce_episode" },
      { from: "produce_episode", to: "collect" },
      { from: "collect", to: "final_review" },
      { from: "final_review", to: "finish" },
      { from: "finish", to: "done" },
    ],
    ...overrides,
  };
}

describe("groupChildrenByNode", () => {
  it("groups children by parent_node_id", () => {
    const children = [
      child({ id: "c1", parent_node_id: "produce_episode" }),
      child({ id: "c2", parent_node_id: "produce_episode" }),
      child({ id: "c3", parent_node_id: "other_subbot" }),
    ];
    const grouped = groupChildrenByNode(children, demoWf());
    expect(grouped.get("produce_episode")?.map((c) => c.id)).toEqual([
      "c1",
      "c2",
    ]);
    expect(grouped.get("other_subbot")?.map((c) => c.id)).toEqual(["c3"]);
  });

  it("attributes legacy children (no parent_node_id) to the single subbot node", () => {
    const children = [child({ id: "c1" }), child({ id: "c2" })];
    const grouped = groupChildrenByNode(children, demoWf());
    expect(grouped.get("produce_episode")?.map((c) => c.id)).toEqual([
      "c1",
      "c2",
    ]);
    expect(grouped.has(UNATTRIBUTED_NODE)).toBe(false);
  });

  it("buckets unattributed children when the wf has no or multiple subbot nodes", () => {
    const noSubbot = demoWf({
      nodes: [
        { id: "plan", kind: "agent" },
        { id: "done", kind: "done" },
      ],
    });
    expect(
      groupChildrenByNode([child({ id: "c1" })], noSubbot).get(
        UNATTRIBUTED_NODE,
      )?.length,
    ).toBe(1);

    const twoSubbots = demoWf({
      nodes: [
        { id: "sb_a", kind: "subbot", source: "a.bot" },
        { id: "sb_b", kind: "subbot", source: "b.bot" },
      ],
    });
    expect(
      groupChildrenByNode([child({ id: "c1" })], twoSubbots).get(
        UNATTRIBUTED_NODE,
      )?.length,
    ).toBe(1);

    // Same for wf-not-yet-loaded.
    expect(
      groupChildrenByNode([child({ id: "c1" })], null).get(UNATTRIBUTED_NODE)
        ?.length,
    ).toBe(1);
  });

  it("never attributes shard children to the single-subbot fallback", () => {
    const children = [
      child({ id: "shard", shard_index: 1, shard_count: 3 }),
      child({ id: "legacy" }),
    ];
    const grouped = groupChildrenByNode(children, demoWf());
    expect(grouped.get(UNATTRIBUTED_NODE)?.map((c) => c.id)).toEqual([
      "shard",
    ]);
    expect(grouped.get("produce_episode")?.map((c) => c.id)).toEqual([
      "legacy",
    ]);
  });

  it("mixes attributed and fallback-attributed children in one pass", () => {
    const children = [
      child({ id: "c1", parent_node_id: "produce_episode" }),
      child({ id: "c2" }), // falls back to the single subbot
    ];
    const grouped = groupChildrenByNode(children, demoWf());
    expect(grouped.get("produce_episode")?.map((c) => c.id)).toEqual([
      "c1",
      "c2",
    ]);
  });
});

describe("childTabLabel", () => {
  it("prefers shard_label, then name, then workflow_name #n", () => {
    expect(
      childTabLabel(child({ shard_label: "ep-1", name: "brave-otter" }), 0),
    ).toBe("ep-1");
    expect(childTabLabel(child({ name: "brave-otter" }), 0)).toBe(
      "brave-otter",
    );
    expect(childTabLabel(child({}), 2)).toBe("episode #3");
  });
});

describe("statusDotClass", () => {
  it("derives solid dot classes from the tone map (running pulses)", () => {
    expect(statusDotClass("running")).toBe("bg-info animate-pulse");
    expect(statusDotClass("paused_waiting_human")).toBe("bg-warning");
    expect(statusDotClass("finished")).toBe("bg-success");
    expect(statusDotClass("failed")).toBe("bg-danger");
    expect(statusDotClass("cancelled")).toBe("bg-fg-subtle");
  });
});

describe("isSettledRunStatus", () => {
  it("is true only for statuses that cannot change without operator action", () => {
    const settled: RunStatus[] = [
      "finished",
      "failed",
      "failed_resumable",
      "cancelled",
    ];
    const open: RunStatus[] = [
      "running",
      "queued",
      "paused_waiting_human",
      "paused_operator",
    ];
    for (const s of settled) expect(isSettledRunStatus(s)).toBe(true);
    for (const s of open) expect(isSettledRunStatus(s)).toBe(false);
  });
});
