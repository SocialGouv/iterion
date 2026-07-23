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
    inFlightToolsByExec: new Map(),
    queuedMessages: [],
    browser: {
      currentUrl: null,
      scope: "external",
      source: null,
      kind: undefined,
      lastEventSeqSeen: null,
      screenshots: [],
      liveSession: null,
    },
    resultLinks: [],
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

// ---------------------------------------------------------------------------
// Per-event-kind coverage — one representative fixture per event type the
// reducer handles. These pin the payload contract consumed from the Go
// emitters (pkg/store/event.go + pkg/runtime, pkg/backend/model/hooks.go)
// ahead of the discriminated-union typing of RunEvent.
// ---------------------------------------------------------------------------

function ts(seq: number): string {
  return `2026-01-01T00:00:${String(seq).padStart(2, "0")}Z`;
}

describe("applyEventsBatch — tool lifecycle (in-flight tools)", () => {
  const execId = "exec:main:implement:0";

  it("registers in-flight tools on tool_started and clears the matching one on tool_called (by tool_use_id)", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("implement", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "tool_started",
        run_id: "run_test",
        branch_id: "main",
        node_id: "implement",
        data: { tool: "Bash", tool_use_id: "t1", input: { command: "ls" } },
      },
      {
        seq: 3,
        timestamp: ts(3),
        type: "tool_started",
        run_id: "run_test",
        branch_id: "main",
        node_id: "implement",
        data: { tool: "Read", tool_use_id: "t2", input: { file_path: "go.mod" } },
      },
    ]);
    let inFlight = runStore.getState().inFlightToolsByExec.get(execId);
    expect(inFlight).toHaveLength(2);
    expect(inFlight![0]!.toolName).toBe("Bash");
    expect(inFlight![1]!.toolUseID).toBe("t2");

    runStore.getState().applyEventsBatch([
      {
        seq: 4,
        timestamp: ts(4),
        type: "tool_called",
        run_id: "run_test",
        branch_id: "main",
        node_id: "implement",
        data: { tool: "Bash", tool_use_id: "t1" },
      },
    ]);
    inFlight = runStore.getState().inFlightToolsByExec.get(execId);
    expect(inFlight).toHaveLength(1);
    expect(inFlight![0]!.toolUseID).toBe("t2");
  });

  it("clears the oldest same-name entry on tool_error when no tool_use_id is carried", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("implement", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "tool_started",
        run_id: "run_test",
        branch_id: "main",
        node_id: "implement",
        data: { tool: "bash", input: { command: "false" } },
      },
      {
        seq: 3,
        timestamp: ts(3),
        type: "tool_error",
        run_id: "run_test",
        branch_id: "main",
        node_id: "implement",
        data: { tool: "bash", error: "exit 1" },
      },
    ]);
    expect(runStore.getState().inFlightToolsByExec.get(execId)).toBeUndefined();
  });

  it("clears in-flight entries for the exec on node_finished", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("implement", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "tool_started",
        run_id: "run_test",
        branch_id: "main",
        node_id: "implement",
        data: { tool: "Bash", tool_use_id: "t1", input: {} },
      },
      nodeFinished("implement", 3),
    ]);
    expect(runStore.getState().inFlightToolsByExec.size).toBe(0);
  });
});

describe("applyEventsBatch — artifact_written", () => {
  it("stamps last_artifact_version from data.version", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("build", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "artifact_written",
        run_id: "run_test",
        branch_id: "main",
        node_id: "build",
        data: { publish: true, version: 3 },
      },
    ]);
    const e = Array.from(runStore.getState().executionsById.values())[0]!;
    expect(e.last_artifact_version).toBe(3);
    expect(e.last_seq).toBe(2);
  });
});

describe("applyEventsBatch — human input lifecycle", () => {
  it("pauses the exec and captures pendingHumanInput on human_input_requested", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("ask", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "human_input_requested",
        run_id: "run_test",
        branch_id: "main",
        node_id: "ask",
        data: {
          interaction_id: "run_test_ask",
          questions: { approve: "Ship it?" },
        },
      },
    ]);
    const st = runStore.getState();
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.status).toBe("paused_waiting_human");
    expect(st.pendingHumanInput).toMatchObject({
      interaction_id: "run_test_ask",
      node_id: "ask",
      questions: { approve: "Ship it?" },
    });
  });

  it("clears pendingHumanInput and resumes the paused exec on run_resumed", () => {
    runStore.getState().applyEventsBatch([
      nodeStarted("ask", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "human_input_requested",
        run_id: "run_test",
        branch_id: "main",
        node_id: "ask",
        data: { interaction_id: "run_test_ask", questions: {} },
      },
      {
        seq: 3,
        timestamp: ts(3),
        type: "run_resumed",
        run_id: "run_test",
      },
    ]);
    const st = runStore.getState();
    expect(st.pendingHumanInput).toBeNull();
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.status).toBe("running");
  });
});

describe("applyEventsBatch — run termination & pause status overrides", () => {
  // Seed a snapshot so runStatusOverride has a base to apply onto.
  function seed() {
    runStore.getState().applySnapshot(snap([], 0));
  }

  it("run_finished closes still-running execs and flips run status to finished", () => {
    seed();
    runStore.getState().applyEventsBatch([
      nodeStarted("work", 1),
      { seq: 2, timestamp: ts(2), type: "run_finished", run_id: "run_test" },
    ]);
    const st = runStore.getState();
    expect(st.snapshot?.run.status).toBe("finished");
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.status).toBe("finished");
    expect(e.finished_at).toBeDefined();
  });

  it("run_failed marks the current exec failed and overrides status to failed_resumable with the error", () => {
    seed();
    runStore.getState().applyEventsBatch([
      nodeStarted("work", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "run_failed",
        run_id: "run_test",
        branch_id: "main",
        node_id: "work",
        data: { error: "budget exhausted", code: "BUDGET_EXCEEDED" },
      },
    ]);
    const st = runStore.getState();
    expect(st.snapshot?.run.status).toBe("failed_resumable");
    expect(st.snapshot?.run.error).toBe("budget exhausted");
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.status).toBe("failed");
    expect(e.error).toBe("budget exhausted");
  });

  it("run_cancelled closes running execs with the reason and sets status cancelled", () => {
    seed();
    runStore.getState().applyEventsBatch([
      nodeStarted("work", 1),
      {
        seq: 2,
        timestamp: ts(2),
        type: "run_cancelled",
        run_id: "run_test",
        data: { reason: "user cancelled" },
      },
    ]);
    const st = runStore.getState();
    expect(st.snapshot?.run.status).toBe("cancelled");
    const e = Array.from(st.executionsById.values())[0]!;
    expect(e.status).toBe("failed");
    expect(e.error).toBe("user cancelled");
  });

  it("run_paused maps operator/cost_cap_daily reasons to paused_operator, default to paused_waiting_human", () => {
    seed();
    runStore.getState().applyEventsBatch([
      {
        seq: 1,
        timestamp: ts(1),
        type: "run_paused",
        run_id: "run_test",
        data: { reason: "operator" },
      },
    ]);
    expect(runStore.getState().snapshot?.run.status).toBe("paused_operator");

    runStore.getState().applyEventsBatch([
      { seq: 2, timestamp: ts(2), type: "run_paused", run_id: "run_test" },
    ]);
    expect(runStore.getState().snapshot?.run.status).toBe(
      "paused_waiting_human",
    );
  });
});

describe("applyEventsBatch — browser events", () => {
  it("tracks the live session across browser_session_started/_ended and ignores a stale end", () => {
    runStore.getState().applyEventsBatch([
      {
        seq: 1,
        timestamp: ts(1),
        type: "browser_session_started",
        run_id: "run_test",
        node_id: "e2e",
        data: { session_id: "s1", node_id: "e2e" },
      },
    ]);
    expect(runStore.getState().browser.liveSession).toMatchObject({
      sessionId: "s1",
      nodeId: "e2e",
    });

    // A stale end for a DIFFERENT session must not clobber the live one.
    runStore.getState().applyEventsBatch([
      {
        seq: 2,
        timestamp: ts(2),
        type: "browser_session_ended",
        run_id: "run_test",
        data: { session_id: "s0" },
      },
    ]);
    expect(runStore.getState().browser.liveSession?.sessionId).toBe("s1");

    runStore.getState().applyEventsBatch([
      {
        seq: 3,
        timestamp: ts(3),
        type: "browser_session_ended",
        run_id: "run_test",
        data: { session_id: "s1" },
      },
    ]);
    expect(runStore.getState().browser.liveSession).toBeNull();
  });

  it("appends browser_screenshot frames with their attachment pointer", () => {
    runStore.getState().applyEventsBatch([
      {
        seq: 1,
        timestamp: ts(1),
        type: "browser_screenshot",
        run_id: "run_test",
        node_id: "e2e",
        data: {
          attachment_name: "shot-1.png",
          url: "http://localhost:5173/",
          source: "playwright",
          tool_call_id: "tc1",
        },
      },
    ]);
    const shots = runStore.getState().browser.screenshots;
    expect(shots).toHaveLength(1);
    expect(shots[0]).toMatchObject({
      seq: 1,
      attachmentName: "shot-1.png",
      url: "http://localhost:5173/",
      toolCallId: "tc1",
      nodeId: "e2e",
    });
  });

  it("adopts preview_url_available with scope/source/kind and records the seq", () => {
    runStore.getState().applyEventsBatch([
      {
        seq: 1,
        timestamp: ts(1),
        type: "preview_url_available",
        run_id: "run_test",
        node_id: "serve",
        data: {
          url: "http://localhost:3000",
          kind: "dev-server",
          scope: "internal",
          source: "tool-stdout",
        },
      },
    ]);
    const b = runStore.getState().browser;
    expect(b.currentUrl).toBe("http://localhost:3000");
    expect(b.scope).toBe("internal");
    expect(b.source).toBe("tool-stdout");
    expect(b.kind).toBe("dev-server");
    expect(b.lastEventSeqSeen).toBe(1);
  });
});

describe("applyEventsBatch — user message inbox", () => {
  it("folds the queued → delivered → consumed lifecycle, merging transition timestamps", () => {
    runStore.getState().applyEventsBatch([
      {
        seq: 1,
        timestamp: ts(1),
        type: "user_message_queued",
        run_id: "run_test",
        data: {
          id: "m1",
          text: "focus on the parser",
          status: "queued",
          queued_at: ts(1),
        },
      },
    ]);
    expect(runStore.getState().queuedMessages).toMatchObject([
      { id: "m1", status: "queued", text: "focus on the parser" },
    ]);

    runStore.getState().applyEventsBatch([
      {
        seq: 2,
        timestamp: ts(2),
        type: "user_message_delivered",
        run_id: "run_test",
        data: {
          id: "m1",
          text: "focus on the parser",
          status: "delivered",
          queued_at: ts(1),
          delivered_at: ts(2),
        },
      },
      {
        seq: 3,
        timestamp: ts(3),
        type: "user_message_consumed",
        run_id: "run_test",
        data: {
          id: "m1",
          text: "focus on the parser",
          status: "consumed",
          queued_at: ts(1),
          delivered_at: ts(2),
          consumed_at: ts(3),
        },
      },
    ]);
    const msgs = runStore.getState().queuedMessages;
    expect(msgs).toHaveLength(1);
    expect(msgs[0]).toMatchObject({
      id: "m1",
      status: "consumed",
      queued_at: ts(1),
      delivered_at: ts(2),
      consumed_at: ts(3),
    });
  });

  it("records a cancellation and drops payloads without an id", () => {
    runStore.getState().applyEventsBatch([
      {
        seq: 1,
        timestamp: ts(1),
        type: "user_message_queued",
        run_id: "run_test",
        data: { id: "m1", text: "wait", status: "queued", queued_at: ts(1) },
      },
      {
        seq: 2,
        timestamp: ts(2),
        type: "user_message_cancelled",
        run_id: "run_test",
        data: {
          id: "m1",
          text: "wait",
          status: "cancelled",
          queued_at: ts(1),
          cancelled_at: ts(2),
        },
      },
      // Malformed payload (no id) — must be ignored, not crash.
      {
        seq: 3,
        timestamp: ts(3),
        type: "user_message_queued",
        run_id: "run_test",
        data: { text: "ghost" },
      },
    ]);
    const msgs = runStore.getState().queuedMessages;
    expect(msgs).toHaveLength(1);
    expect(msgs[0]).toMatchObject({
      id: "m1",
      status: "cancelled",
      cancelled_at: ts(2),
    });
  });
});

describe("applyLogChunk — byte-keyed overlap/dedup", () => {
  beforeEach(() => runStore.getState().clearLog());

  it("appends contiguous chunks and tracks byte cursor with multi-byte glyphs", () => {
    const s = runStore.getState();
    s.applyLogChunk({ offset: 0, text: "🔧a" }); // 🔧=4 bytes, a=1 → 5 bytes
    let log = runStore.getState().log;
    expect(log.text).toBe("🔧a");
    expect(log.nextByte).toBe(5);

    runStore.getState().applyLogChunk({ offset: 5, text: "b" });
    log = runStore.getState().log;
    expect(log.text).toBe("🔧ab");
    expect(log.nextByte).toBe(6);
  });

  it("drops a fully-overlapping resend without duplicating", () => {
    runStore.getState().applyLogChunk({ offset: 0, text: "🔧ab" }); // 6 bytes
    runStore.getState().applyLogChunk({ offset: 0, text: "🔧ab" }); // exact resend
    expect(runStore.getState().log.text).toBe("🔧ab");
  });

  it("skips the overlapping byte prefix on a partial resend after reconnect", () => {
    // Server resends a tail starting inside the multi-byte-preceded region.
    runStore.getState().applyLogChunk({ offset: 0, text: "🔧abc" }); // bytes: 4+1+1+1 = 7
    // Reconnect resends from byte 5 ("bc") plus new "d".
    runStore.getState().applyLogChunk({ offset: 5, text: "bcd" });
    const log = runStore.getState().log;
    expect(log.text).toBe("🔧abcd"); // no dup of "bc", "d" appended
    expect(log.nextByte).toBe(8);
  });
});

// previewEvent builds a preview_url_available event as the runtime emits
// one from a tool node's `[iterion] preview_url=…` stdout directive.
function previewEvent(
  seq: number,
  url: string,
  kind?: string,
  scope: "internal" | "external" = "external",
): RunEvent {
  return {
    seq,
    timestamp: ts(seq),
    type: "preview_url_available",
    run_id: "run_test",
    branch_id: "main",
    node_id: "surface_pr_link",
    data: { url, kind, scope, source: "tool-stdout" },
  };
}

describe("applyEventsBatch — result-links (headline PR/deploy surfacing)", () => {
  it("captures a kind=pr preview_url as a visible result-link", () => {
    runStore.getState().applyEventsBatch([
      previewEvent(1, "https://github.com/o/r/pull/7", "pr"),
    ]);
    const { resultLinks } = runStore.getState();
    expect(resultLinks).toHaveLength(1);
    expect(resultLinks[0]).toMatchObject({
      url: "https://github.com/o/r/pull/7",
      kind: "pr",
      scope: "external",
    });
  });

  it("does NOT embed a PR link in the Browser pane (GitHub blocks iframing)", () => {
    runStore.getState().applyEventsBatch([
      previewEvent(1, "https://github.com/o/r/pull/7", "pr"),
    ]);
    const { browser } = runStore.getState();
    // The PR surfaces as a result-link, never as the pane's currentUrl…
    expect(browser.currentUrl).toBeNull();
    // …but the seen-seq still advances so the pane's auto-reveal logic
    // stays consistent.
    expect(browser.lastEventSeqSeen).toBe(1);
  });

  it("keeps an embeddable deploy URL in the Browser pane AND as a result-link", () => {
    runStore.getState().applyEventsBatch([
      previewEvent(1, "https://app.example.com", "deploy"),
    ]);
    const st = runStore.getState();
    expect(st.browser.currentUrl).toBe("https://app.example.com");
    expect(st.browser.kind).toBe("deploy");
    expect(st.resultLinks).toHaveLength(1);
    expect(st.resultLinks[0]).toMatchObject({ kind: "deploy" });
  });

  it("does NOT capture a non-result kind (dev-server) as a result-link", () => {
    runStore.getState().applyEventsBatch([
      previewEvent(1, "http://localhost:5173", "dev-server"),
    ]);
    const st = runStore.getState();
    expect(st.resultLinks).toHaveLength(0);
    // …but it still drives the Browser pane.
    expect(st.browser.currentUrl).toBe("http://localhost:5173");
  });

  it("holds both a deploy and a PR link, deduped by url, in discovery order", () => {
    // app-dev shape: deploy surfaces first, the PR after the MR tail.
    runStore.getState().applyEventsBatch([
      previewEvent(1, "https://app.example.com", "deploy"),
      previewEvent(2, "https://github.com/o/r/pull/7", "pr"),
      // A duplicate PR directive (idempotent re-run) must not double it.
      previewEvent(3, "https://github.com/o/r/pull/7", "pr"),
    ]);
    const { resultLinks } = runStore.getState();
    expect(resultLinks.map((l) => l.kind)).toEqual(["deploy", "pr"]);
    expect(resultLinks.map((l) => l.url)).toEqual([
      "https://app.example.com",
      "https://github.com/o/r/pull/7",
    ]);
  });

  it("reconstructs result-links on a reloaded terminal run (event-log replay)", () => {
    // A finished run: cold-loaded snapshot at the final seq, then the full
    // event log hydrated via loadEventHistoryIfMissing (applyEventsBatch).
    const finished = {
      ...baseRun,
      status: "finished",
    } as unknown as RunHeader;
    runStore.getState().applySnapshot({ run: finished, executions: [], last_seq: 3 });
    // Snapshots carry no result-links; the replayed history rebuilds them.
    expect(runStore.getState().resultLinks).toHaveLength(0);
    runStore.getState().applyEventsBatch([
      nodeStarted("finalize_mr", 1),
      nodeFinished("finalize_mr", 2),
      previewEvent(3, "https://github.com/o/r/pull/7", "pr"),
    ]);
    const { resultLinks } = runStore.getState();
    expect(resultLinks).toHaveLength(1);
    expect(resultLinks[0]).toMatchObject({
      url: "https://github.com/o/r/pull/7",
      kind: "pr",
    });
  });

  it("ignores an empty-url preview event", () => {
    runStore.getState().applyEventsBatch([previewEvent(1, "", "pr")]);
    expect(runStore.getState().resultLinks).toHaveLength(0);
  });
});
