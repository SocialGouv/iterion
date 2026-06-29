import { beforeEach, describe, expect, it } from "vitest";
import { runStore } from "./run";
import type { RunSnapshot, RunEvent, RunHeader, ExecutionState } from "@/api/runs";

const baseRun: RunHeader = {
  id: "run_test",
  name: "test",
  workflow: "wf",
  status: "running",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
} as unknown as RunHeader;

function exec(node: string, status: ExecutionState["status"], iter = 0, seq = 0): ExecutionState {
  return {
    execution_id: `exec:main:${node}:${iter}`,
    ir_node_id: node,
    branch_id: "main",
    loop_iteration: iter,
    status,
    current_event_seq: seq,
    first_seq: seq,
    last_seq: seq,
  } as ExecutionState;
}

function snap(executions: ExecutionState[], last_seq: number): RunSnapshot {
  return { run: baseRun, executions, last_seq };
}

function nodeStarted(node: string, seq: number, iter = 0): RunEvent {
  return {
    seq,
    timestamp: "2026-01-01T00:00:01Z",
    type: "node_started",
    run_id: "run_test",
    branch_id: "main",
    node_id: node,
    // Pass iteration explicitly so the reducer doesn't auto-bump it
    // via nextIteration() — the bug fix targets duplicate events for
    // the SAME iter, which is what WS history replay produces. The
    // nextIteration fallback (which legitimately bumps for recovery
    // retries) is a different code path that already creates a new
    // exec id.
    data: { kind: "agent", iteration: iter },
  };
}

function nodeFinished(node: string, seq: number): RunEvent {
  return {
    seq,
    timestamp: "2026-01-01T00:00:02Z",
    type: "node_finished",
    run_id: "run_test",
    branch_id: "main",
    node_id: node,
  };
}

beforeEach(() => {
  // Reset to a known empty state. Cast to never to bypass the
  // partial-shape requirement since we want a full wipe per test.
  runStore.setState({
    snapshot: null,
    executionsById: new Map(),
    lastExecIDByNode: new Map(),
    events: [],
    pendingHumanInput: null,
    latestTodosByExec: new Map(),
    todoHistoryByExec: new Map(),
  } as never);
});

describe("applySnapshot", () => {
  it("populates executions and snapshot on first call", () => {
    const s = snap([exec("detect_stack", "running", 0, 1)], 1);
    runStore.getState().applySnapshot(s);
    const st = runStore.getState();
    expect(st.snapshot?.last_seq).toBe(1);
    expect(st.executionsById.size).toBe(1);
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.ir_node_id).toBe("detect_stack");
    expect(e.status).toBe("running");
  });

  // Regression: REST and WS each push a snapshot. If the older one
  // arrives second, it must NOT overwrite the newer state — that was
  // the dominant root cause of "two nodes show as running" (the
  // finished node's transition was clobbered by stale snapshot data).
  it("ignores a stale snapshot whose last_seq is older than the current one", () => {
    const newer = snap([exec("detect_stack", "finished", 0, 3), exec("discover_outdated", "running", 0, 5)], 5);
    runStore.getState().applySnapshot(newer);

    const stale = snap([exec("detect_stack", "running", 0, 1)], 1);
    runStore.getState().applySnapshot(stale);

    const st = runStore.getState();
    expect(st.snapshot?.last_seq).toBe(5);
    expect(st.executionsById.size).toBe(2);
    const detect = Array.from(st.executionsById.values()).find((e) => e.ir_node_id === "detect_stack");
    expect(detect?.status).toBe("finished");
  });

  // Regression: events that arrived between the snapshot's last_seq
  // and the snapshot being applied must be re-applied on top of the
  // snapshot's base. Without this, the second-arriving newer event
  // (e.g. detect_stack node_finished) was dropped — leaving the UI
  // showing both detect_stack AND discover_outdated as running.
  it("re-applies events that are newer than the snapshot's last_seq", () => {
    // Simulate: WS pushes node_started(detect_stack)@1 and
    // node_finished(detect_stack)@2 and node_started(discover_outdated)@3
    // BEFORE the REST snapshot resolves (carrying state at seq=1).
    runStore.getState().applyEventsBatch([
      nodeStarted("detect_stack", 1),
      nodeFinished("detect_stack", 2),
      nodeStarted("discover_outdated", 3),
    ]);
    // Pre-condition: store has detect_stack=finished + discover_outdated=running.
    {
      const st = runStore.getState();
      const detect = Array.from(st.executionsById.values()).find((e) => e.ir_node_id === "detect_stack");
      const discover = Array.from(st.executionsById.values()).find((e) => e.ir_node_id === "discover_outdated");
      expect(detect?.status).toBe("finished");
      expect(discover?.status).toBe("running");
    }

    // Now an OLDER REST snapshot lands (server saw only seq=1, so
    // detect_stack appears "running" and discover_outdated is absent).
    const restSnapshot = snap([exec("detect_stack", "running", 0, 1)], 1);
    runStore.getState().applySnapshot(restSnapshot);

    // The newer events (seq=2, seq=3) must have been re-applied to
    // the snapshot's base. State must reflect the latest known
    // truth, not the stale snapshot.
    const st = runStore.getState();
    const detect = Array.from(st.executionsById.values()).find((e) => e.ir_node_id === "detect_stack");
    const discover = Array.from(st.executionsById.values()).find((e) => e.ir_node_id === "discover_outdated");
    expect(detect?.status).toBe(
      "finished",
    );
    expect(discover?.status).toBe(
      "running",
    );
  });
});

describe("applyEventsBatch — monotonic status", () => {
  // Regression for the "finished node continues to show running"
  // glitch. A duplicate node_started for an exec id that already
  // reached node_finished must NOT downgrade the status back to
  // running, and must NOT clear finished_at. The runtime can re-emit
  // node_started in legit cases (WS history replay on reconnect,
  // recovery retry that reuses the same iteration) — the reducer is
  // responsible for treating terminal statuses as immutable in this
  // direction.
  it("does not downgrade finished -> running on duplicate node_started", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("detect_stack", 1),
      nodeFinished("detect_stack", 2),
    ]);
    {
      const st = runStore.getState();
      const e = Array.from(st.executionsById.values())[0]!;
      expect(e.status).toBe("finished");
      expect(e.finished_at).toBeDefined();
    }

    // Duplicate node_started arrives (history replay or server
    // re-emission). Higher seq than the prior finished — the
    // dedupe filter at the top of the reducer doesn't drop it.
    runStore.getState().applyEventsBatch([nodeStarted("detect_stack", 3)]);

    const st = runStore.getState();
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.status).toBe("finished");
    expect(e.finished_at).toBeDefined();
    expect(e.last_seq).toBe(3); // seq markers still advance
  });

  it("does not downgrade failed -> running on duplicate node_started", () => {
    // Build the failed state through the public API: node_started ->
    // run_failed. run_failed flips the current exec to status=failed.
    runStore.getState().applyEventsBatch([
      nodeStarted("validate", 1),
      {
        seq: 2,
        timestamp: "2026-01-01T00:00:02Z",
        type: "run_failed",
        run_id: "run_test",
        branch_id: "main",
        node_id: "validate",
        data: { error: "boom" },
      } as RunEvent,
    ]);
    {
      const st = runStore.getState();
      const e = Array.from(st.executionsById.values())[0]!;
      expect(e.status).toBe("failed");
    }

    runStore.getState().applyEventsBatch([nodeStarted("validate", 3)]);
    const st = runStore.getState();
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.status).toBe("failed");
  });
});

describe("applyEventsBatch — nested-loop exec_id attribution", () => {
  // Reproduces the post-Option-3 "half the nodes show as running" bug.
  // Two consecutive package iterations of the same node emit
  // node_started events keyed on distinct iteration_path values
  // (package_loop=11 vs package_loop=12) but the SAME scalar
  // `iteration`. The reducer must route each node_finished to the
  // node_started that immediately preceded it — not to the highest
  // scalar loop_iteration entry, which was the legacy heuristic and
  // is non-deterministic when distinct execs share loop_iteration.
  it("routes node_finished by lastExecIDByNode, not max(loop_iteration)", () => {
    const path11 = "family_loop=5;fix_loop=0;package_loop=11";
    const path12 = "family_loop=5;fix_loop=0;package_loop=12";
    const evtStarted = (path: string, seq: number, iter: number): RunEvent => ({
      seq,
      timestamp: `2026-01-01T00:00:0${seq}Z`,
      type: "node_started",
      run_id: "run_test",
      branch_id: "main",
      node_id: "validate_upgrade",
      data: { kind: "judge", iteration: iter, iteration_path: path },
    } as RunEvent);
    const evtFinished = (seq: number): RunEvent => ({
      seq,
      timestamp: `2026-01-01T00:00:0${seq}Z`,
      type: "node_finished",
      run_id: "run_test",
      branch_id: "main",
      node_id: "validate_upgrade",
    } as RunEvent);

    runStore.getState().applyEventsBatch([
      evtStarted(path11, 1, 0),
      evtFinished(2),
      evtStarted(path12, 3, 0),
      evtFinished(4),
    ]);

    const st = runStore.getState();
    const execs = Array.from(st.executionsById.values()).filter(
      (e) => e.ir_node_id === "validate_upgrade",
    );
    expect(execs).toHaveLength(2);
    // Both attempts MUST be terminal. Pre-fix, currentExec's
    // max(loop_iteration) scan was non-deterministic across map order
    // since both shared loop_iteration=0; node_finished could land on
    // either, leaving the other forever "running".
    for (const e of execs) {
      expect(e.status).toBe("finished");
    }
    // Verify the lastExecIDByNode map points at the LATEST
    // node_started (pkg=12) so any further downstream event in this
    // (branch, node) attributes there.
    const recorded = st.lastExecIDByNode.get("main\tvalidate_upgrade");
    expect(recorded).toBe(`exec:main:validate_upgrade:${path12}`);
  });
});

// ---------------------------------------------------------------------------
// Session board (Tasks tab) — persistent task-list history
// ---------------------------------------------------------------------------

import { selectActiveTodos, selectTodoTimeline } from "./run";

function todoWrite(
  node: string,
  seq: number,
  todos: { content: string; status: string; activeForm?: string }[],
  tool = "TodoWrite",
): RunEvent {
  return {
    seq,
    timestamp: `2026-01-01T00:00:${String(seq).padStart(2, "0")}Z`,
    type: "tool_started",
    run_id: "run_test",
    branch_id: "main",
    node_id: node,
    data: { tool, input: { todos } },
  };
}

describe("session board todo history", () => {
  it("accumulates a per-execution history, deduping consecutive identical lists", () => {
    const list1 = [
      { content: "Plan", status: "in_progress", activeForm: "Planning" },
      { content: "Build", status: "pending" },
    ];
    runStore.getState().applyEventsBatch([
      nodeStarted("implement", 1),
      todoWrite("implement", 2, list1),
      // identical → collapsed
      todoWrite("implement", 3, list1),
    ]);
    let timeline = selectTodoTimeline(runStore.getState());
    expect(timeline).toHaveLength(1);
    expect(timeline[0]!.snapshots).toHaveLength(1);

    // A real change appends a second snapshot.
    runStore.getState().applyEventsBatch([
      todoWrite("implement", 4, [
        { content: "Plan", status: "completed" },
        { content: "Build", status: "in_progress", activeForm: "Building" },
      ]),
    ]);
    timeline = selectTodoTimeline(runStore.getState());
    expect(timeline[0]!.snapshots).toHaveLength(2);
    expect(timeline[0]!.latest.todos[1]!.status).toBe("in_progress");
  });

  it("survives node_finished and run_finished (unlike the live snapshot)", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("implement", 1),
      todoWrite("implement", 2, [{ content: "Work", status: "in_progress" }]),
      nodeFinished("implement", 3),
    ]);
    // Live snapshot is cleared on finish; the board history is not.
    expect(selectActiveTodos(runStore.getState())).toBeNull();
    expect(selectTodoTimeline(runStore.getState())).toHaveLength(1);

    runStore.getState().applyEventsBatch([
      { seq: 4, timestamp: "2026-01-01T00:00:04Z", type: "run_finished", run_id: "run_test" },
    ]);
    const timeline = selectTodoTimeline(runStore.getState());
    expect(timeline).toHaveLength(1);
    expect(timeline[0]!.exec?.ir_node_id).toBe("implement");
  });

  it("orders multiple executions chronologically by start time", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("plan", 1),
      todoWrite("plan", 2, [{ content: "A", status: "completed" }]),
      nodeStarted("implement", 3),
      todoWrite("implement", 4, [{ content: "B", status: "in_progress" }], "todo_write"),
    ]);
    const timeline = selectTodoTimeline(runStore.getState());
    expect(timeline.map((e) => e.exec?.ir_node_id)).toEqual(["plan", "implement"]);
  });
});
