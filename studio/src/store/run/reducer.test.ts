import { describe, expect, it } from "vitest";

import type { ExecutionState, RunEvent, RunSnapshot } from "@/api/runs";

import { execKey, reduceEvents } from "./reducer";

// Minimal ReduceInput state: one finished exec for `analyze` (pre-pivot)
// and one finished exec for `verify` (rewind pivot), plus the lookup
// pointer node_started would have stamped.
function baseExec(node: string, seq: number): ExecutionState {
  return {
    execution_id: `exec:main:${node}:0`,
    ir_node_id: node,
    branch_id: "main",
    loop_iteration: 0,
    status: "finished",
    started_at: "2026-05-14T12:00:00Z",
    finished_at: "2026-05-14T12:00:30Z",
    current_event_seq: seq,
    first_seq: seq - 1,
    last_seq: seq,
  };
}

function baseSnapshot(): RunSnapshot {
  return {
    run: {
      id: "r1",
      workflow_name: "wf",
      status: "failed_resumable",
      created_at: "2026-05-14T12:00:00Z",
      updated_at: "2026-05-14T12:00:30Z",
      active_duration_ms: 30_000,
    },
    executions: [baseExec("analyze", 2), baseExec("verify", 4)],
    last_seq: 4,
  };
}

function baseState() {
  const executionsById = new Map<string, ExecutionState>();
  for (const e of baseSnapshot().executions) {
    executionsById.set(e.execution_id, e);
  }
  return {
    events: [] as RunEvent[],
    executionsById,
    lastExecIDByNode: new Map([
      [execKey("main", "analyze"), "exec:main:analyze:0"],
      [execKey("main", "verify"), "exec:main:verify:0"],
    ]),
    inFlightToolsByExec: new Map(),
    latestTodosByExec: new Map(),
    todoHistoryByExec: new Map(),
    snapshot: baseSnapshot() as RunSnapshot | null,
    pendingHumanInput: null as {
      interaction_id?: string;
      node_id?: string;
      questions?: Record<string, unknown>;
    } | null,
    queuedMessages: [],
    browser: {
      currentUrl: null,
      scope: "external" as const,
      source: null,
      kind: undefined,
      lastEventSeqSeen: null,
      screenshots: [],
      liveSession: null,
    },
    resultLinks: [],
  };
}

function rewindEvent(seq: number, dropped: string[]): RunEvent {
  return {
    seq,
    timestamp: "2026-05-14T12:05:00Z",
    type: "run_rewound",
    run_id: "r1",
    branch_id: "main",
    node_id: "verify",
    data: { dropped_nodes: dropped, to_node: "verify" },
  };
}

describe("reduceEvents run_rewound", () => {
  it("erases the dropped nodes' execs and keeps the pre-pivot ones", () => {
    const next = reduceEvents(baseState(), [rewindEvent(5, ["verify"])]);
    const execs = [...(next.executionsById?.values() ?? [])];
    expect(execs).toHaveLength(1);
    expect(execs[0]?.ir_node_id).toBe("analyze");
    // The snapshot's ordered executions reflect the deletion too.
    expect(next.snapshot?.executions).toHaveLength(1);
    expect(next.snapshot?.executions[0]?.ir_node_id).toBe("analyze");
    // The (branch, node) → exec_id pointer of the dropped node is gone.
    expect(next.lastExecIDByNode?.has(execKey("main", "verify"))).toBe(false);
    expect(next.lastExecIDByNode?.has(execKey("main", "analyze"))).toBe(true);
  });

  it("parks the run as cancelled even when no exec matches", () => {
    const next = reduceEvents(baseState(), [rewindEvent(5, ["nosuchnode"])]);
    expect(next.snapshot?.run.status).toBe("cancelled");
    expect(next.snapshot?.executions).toHaveLength(2);
  });

  it("drops a pending pause form belonging to a dropped node", () => {
    const state = baseState();
    state.pendingHumanInput = {
      interaction_id: "i1",
      node_id: "verify",
      questions: {},
    };
    const next = reduceEvents(state, [rewindEvent(5, ["verify"])]);
    expect(next.pendingHumanInput).toBeNull();
  });

  it("keeps a pending pause form of a surviving node", () => {
    const state = baseState();
    state.pendingHumanInput = {
      interaction_id: "i1",
      node_id: "analyze",
      questions: {},
    };
    const next = reduceEvents(state, [rewindEvent(5, ["verify"])]);
    expect(next.pendingHumanInput).toBeUndefined();
  });

  it("a post-rewind node_started recreates a clean running exec", () => {
    const state = baseState();
    const after = reduceEvents(state, [
      rewindEvent(5, ["verify"]),
      {
        seq: 6,
        timestamp: "2026-05-14T12:06:00Z",
        type: "run_resumed",
        run_id: "r1",
        branch_id: "main",
      },
      {
        seq: 7,
        timestamp: "2026-05-14T12:06:01Z",
        type: "node_started",
        run_id: "r1",
        branch_id: "main",
        node_id: "verify",
      },
    ]);
    const execs = [...(after.executionsById?.values() ?? [])];
    const verify = execs.find((e) => e.ir_node_id === "verify");
    expect(execs).toHaveLength(2);
    expect(verify?.status).toBe("running");
    expect(verify?.finished_at).toBeUndefined();
    expect(after.snapshot?.executions).toHaveLength(2);
  });
});
