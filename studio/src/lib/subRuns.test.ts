import { describe, expect, it } from "vitest";

import type { RunStatus, RunSummary, WireWorkflow } from "@/api/runs";
import {
  childTabLabel,
  groupChildrenByNode,
  isSettledRunStatus,
  resolveSelectedSubbotChild,
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

describe("resolveSelectedSubbotChild", () => {
  it("returns undefined for an empty list", () => {
    expect(resolveSelectedSubbotChild([])).toBeUndefined();
  });

  it("defaults to the later finished child over an earlier failed one (list order)", () => {
    const children = [
      child({ id: "old-fail", status: "failed" }),
      child({ id: "new-pass", status: "finished" }),
    ];
    expect(resolveSelectedSubbotChild(children)).toBe("new-pass");
  });

  it("tie-breaks equal rank by later array position, not ISO string order", () => {
    // Go encoding/json RFC3339Nano trims fractional zeros, so ".12Z" >
    // ".125Z" lexicographically even though 120ms is older than 125ms.
    const children = [
      child({
        id: "older",
        status: "finished",
        created_at: "2026-08-01T10:00:00.12Z",
      }),
      child({
        id: "newer",
        status: "finished",
        created_at: "2026-08-01T10:00:00.125Z",
      }),
    ];
    expect(resolveSelectedSubbotChild(children)).toBe("newer");
  });

  it("prefers a running child over settled historical children", () => {
    const children = [
      child({ id: "old-fail", status: "failed" }),
      child({ id: "live", status: "running" }),
      child({ id: "new-pass", status: "finished" }),
    ];
    expect(resolveSelectedSubbotChild(children)).toBe("live");
  });

  it("prefers paused_waiting_human over settled children, but not over running", () => {
    const waiting = [
      child({ id: "old-fail", status: "failed" }),
      child({ id: "gate", status: "paused_waiting_human" }),
      child({ id: "new-pass", status: "finished" }),
    ];
    expect(resolveSelectedSubbotChild(waiting)).toBe("gate");

    const runningWins = [
      child({ id: "gate", status: "paused_waiting_human" }),
      child({ id: "live", status: "running" }),
    ];
    expect(resolveSelectedSubbotChild(runningWins)).toBe("live");
  });

  it("prefers another unsettled child (queued / operator pause) over settled history", () => {
    const children = [
      child({ id: "old-fail", status: "failed" }),
      child({ id: "queued", status: "queued" }),
      child({ id: "new-pass", status: "finished" }),
    ];
    expect(resolveSelectedSubbotChild(children)).toBe("queued");
  });

  it("keeps a valid explicit user selection", () => {
    const children = [
      child({ id: "old-fail", status: "failed" }),
      child({ id: "live", status: "running" }),
      child({ id: "new-pass", status: "finished" }),
    ];
    expect(resolveSelectedSubbotChild(children, "old-fail")).toBe("old-fail");
  });

  it("drops a stale explicit selection and falls back to the live/latest child", () => {
    const children = [
      child({ id: "old-fail", status: "failed" }),
      child({ id: "new-pass", status: "finished" }),
    ];
    expect(resolveSelectedSubbotChild(children, "gone")).toBe("new-pass");
  });

  it("does not let a historical fan-out failure paint the current pipeline", () => {
    // fan_out_each × subbot: children arrive created_at asc, one per batch.
    const children = [
      child({
        id: "batch-0-fail",
        status: "failed",
        parent_node_id: "review_epic_acceptance",
      }),
      child({
        id: "batch-1-pass",
        status: "finished",
        parent_node_id: "review_epic_acceptance",
      }),
      child({
        id: "batch-2-pass",
        status: "finished",
        parent_node_id: "review_epic_acceptance",
      }),
    ];
    expect(resolveSelectedSubbotChild(children)).toBe("batch-2-pass");
  });

  it("keeps sticky when statuses change and length is unchanged", () => {
    const first = [
      child({ id: "a", status: "running" }),
      child({ id: "b", status: "running" }),
      child({ id: "c", status: "queued" }),
    ];
    expect(resolveSelectedSubbotChild(first)).toBe("b");
    const after = [
      child({ id: "a", status: "finished" }),
      child({ id: "b", status: "finished" }),
      child({ id: "c", status: "finished" }),
    ];
    expect(
      resolveSelectedSubbotChild(after, undefined, { id: "b", count: 3 }),
    ).toBe("b");
  });

  it("drops sticky when the list grows (new spawn, #525)", () => {
    const children = [
      child({ id: "old-fail", status: "failed" }),
      child({ id: "new-pass", status: "finished" }),
    ];
    expect(
      resolveSelectedSubbotChild(children, undefined, {
        id: "old-fail",
        count: 1,
      }),
    ).toBe("new-pass");
  });

  it("drops sticky when that child disappears", () => {
    const children = [
      child({ id: "a", status: "failed" }),
      child({ id: "b", status: "finished" }),
    ];
    expect(
      resolveSelectedSubbotChild(children, undefined, {
        id: "gone",
        count: 2,
      }),
    ).toBe("b");
  });

  it("lets an explicit pick win over sticky", () => {
    const children = [
      child({ id: "a", status: "failed" }),
      child({ id: "b", status: "running" }),
    ];
    expect(
      resolveSelectedSubbotChild(children, "a", { id: "b", count: 2 }),
    ).toBe("a");
  });
});
