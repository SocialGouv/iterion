import { describe, it, expect } from "vitest";
import type { RunAttempt, RunEvent, RunHeader, RunSnapshot, WireWorkflow } from "@/api/runs";
import { createRunStore } from "@/store/run";
import { computeEventDrivenMetrics } from "@/hooks/useRunMetrics";
import { attemptChildren, attemptExecutions, scopeAttempt, withExecutionNodes } from "./runAttempt";

const stamp = "2026-10-25T10:00:00Z";
const a: RunAttempt = { id: "a", started_at: stamp, workflow_hash: "old", budget: { max_tokens: 100 } };
const b: RunAttempt = { id: "b", started_at: stamp, workflow_hash: "new", budget: { max_tokens: 200 } };
function event(seq: number, type: string, node_id = "", data = {}): RunEvent {
  return { run_id: "r", seq, timestamp: stamp, type, node_id, data } as RunEvent;
}
const events = [
  event(0, "run_started", "", { attempt: a }),
  event(1, "node_started", "work", { iteration: 0, kind: "tool" }),
  event(2, "node_finished", "work", { output: { _tokens: 10, _cost_usd: 1 } }),
  event(3, "budget_exceeded"),
  event(4, "run_failed", "work", { error: "budget" }),
  event(5, "run_resumed", "", { attempt: b }),
  event(6, "node_started", "work", { iteration: 0, kind: "tool" }),
  event(7, "node_finished", "work", { output: { _tokens: 20, _cost_usd: 2 } }),
  event(8, "node_started", "added", { iteration: 0, kind: "tool" }),
];

describe("#2150 attempt console", () => {
  // Mutation: scope all events (remove the start bound). Old cost, budget
  // failure and execution status then contaminate the currently running attempt.
  it("isolates metrics and the repeated execution ID at a sequence boundary", () => {
    const current = scopeAttempt(events, [a, b], null);
    const execs = attemptExecutions(current, new Map());
    const metrics = computeEventDrivenMetrics(current.events, execs);
    expect(metrics).toMatchObject({ totalTokens: 20, costUsd: 2, budgetExceeded: false, failedCount: 0, branchCountActive: 1 });
    expect([...execs.values()].find((e) => e.ir_node_id === "added")?.status).toBe("running");
    const old = scopeAttempt(events, [a, b], "a");
    const oldMetrics = computeEventDrivenMetrics(old.events, attemptExecutions(old, execs));
    expect(oldMetrics).toMatchObject({ totalTokens: 10, costUsd: 1, budgetExceeded: true, branchCountActive: 0 });
    expect(old.attempt?.budget?.max_tokens).toBe(100);
    expect(current.attempt?.budget?.max_tokens).toBe(200);
    expect(old.key).not.toBe(current.key);
  });

  // Mutation: remove the live attempt metadata fold in reduceEvents. This
  // leaves the old query key and old meter denominator until a REST refresh.
  it("switches provenance and caps on the live resume event", () => {
    const st = createRunStore();
    const run = { id: "r", workflow_name: "w", status: "failed_resumable", attempts: [a], budget: a.budget } as RunHeader;
    st.getState().applySnapshot({ run, executions: [], last_seq: 4 } as RunSnapshot);
    st.getState().applyEvent(events[5]!);
    expect(st.getState().snapshot?.run.budget?.max_tokens).toBe(200);
    expect(st.getState().snapshot?.run.attempts?.map((x) => x.id)).toEqual(["a", "b"]);
    expect(st.getState().snapshot?.run.workflow_hash).toBe("new");
  });

  // Mutation: return wf unchanged from withExecutionNodes. A legacy graph
  // without an observed running node must still show that execution.
  it("keeps an observed active node visible in incomplete legacy provenance", () => {
    const scope = scopeAttempt(events, [a, b], null);
    const executions = [...attemptExecutions(scope, new Map()).values()];
    const graph = { name: "w", entry: "work", nodes: [{ id: "work", kind: "tool" }], edges: [] } as WireWorkflow;
    expect(withExecutionNodes(graph, executions).nodes.map((n) => n.id)).toEqual(["work", "added"]);
    expect(graph.nodes).toHaveLength(1);
  });

  it("respects attempt boundaries for legacy event streams too", () => {
    const legacy = events.map((e) => e.type === "run_started" || e.type === "run_resumed" ? { ...e, data: {} } : e);
    expect(scopeAttempt(legacy, [], null).events[0]?.seq).toBe(5);
  });
});

// Mutation: restore MAX_EVENTS slicing in reduceEvents. The historical cost
// vanishes after the current attempt emits enough events.
it("retains historical metrics after more than 5000 current events", () => {
  const st = createRunStore();
  st.getState().applyEventsBatch([...events, ...Array.from({ length: 5001 }, (_, i) => event(i + 9, "tool_called"))]);
  const scope = scopeAttempt(st.getState().events, [a, b], "a");
  expect(computeEventDrivenMetrics(scope.events, attemptExecutions(scope, st.getState().executionsById)).costUsd).toBe(1);
});

// Mutation: group all childRuns in RunView without attemptChildren. The
// historical frame then selects the newest child belonging to the resume.
it("separates subbot children at the attempt boundary", () => {
  const next = { ...b, started_at: "2026-10-25T10:01:00Z" };
  const children = [
    { id: "old", created_at: "2026-10-25T10:00:01Z" },
    { id: "new", created_at: "2026-10-25T10:01:01Z" },
  ] as import("@/api/runs").RunSummary[];
  expect(attemptChildren(children, scopeAttempt([], [a, next], "a")).map((c) => c.id)).toEqual(["old"]);
  expect(attemptChildren(children, scopeAttempt([], [a, next], null)).map((c) => c.id)).toEqual(["new"]);
});

// Mutation: use creation time alone, ignoring the persisted reattachment map.
it("keeps a reattached child in the currently executing attempt", () => {
  const next = { ...b, started_at: "2026-10-25T10:01:00Z" };
  const children = [{ id: "reattached", created_at: "2026-10-25T10:00:01Z" }] as import("@/api/runs").RunSummary[];
  expect(attemptChildren(children, scopeAttempt([], [a, next], null), ["reattached"]).map((c) => c.id)).toEqual(["reattached"]);
});

// Mutation: compare children with Date.parse alone (millisecond truncation).
it("preserves sub-millisecond child creation boundaries", () => {
  const next = { ...b, started_at: "2026-10-25T10:00:01.000500000Z" };
  const children = [{ id: "before", created_at: "2026-10-25T10:00:01.000400000Z" }] as import("@/api/runs").RunSummary[];
  expect(attemptChildren(children, scopeAttempt([], [a, next], null))).toEqual([]);
  expect(attemptChildren(children, scopeAttempt([], [a, next], "a")).map((c) => c.id)).toEqual(["before"]);
});

// Mutation: rely only on run.subbot_children, whose pointer clears on success.
it("retains consumed child relations from this attempt's events", () => {
  const next = { ...b, started_at: "2026-10-25T10:01:00Z" };
  const stream = [event(10, "run_resumed", "", { attempt: next }), event(11, "subbot_attached", "sub", { child_run_id: "reused" })];
  const children = [{ id: "reused", created_at: "2026-10-25T10:00:01Z" }] as import("@/api/runs").RunSummary[];
  expect(attemptChildren(children, scopeAttempt(stream, [a, next], null), []).map((c) => c.id)).toEqual(["reused"]);
});
