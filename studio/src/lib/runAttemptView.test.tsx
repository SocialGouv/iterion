// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { createElement } from "react";
import { act, cleanup, render, renderHook } from "@testing-library/react";
import { useInlineSubbotData } from "@/hooks/useInlineSubbotData";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { RunAttempt, RunEvent, RunHeader, RunSnapshot } from "@/api/runs";
import { runStore } from "@/store/run";
import { BudgetSection } from "@/components/Runs/leftPanel/overview/BudgetSection";
import { useRunMetrics } from "@/hooks/useRunMetrics";
import { useNodeLabel } from "./runChat/useNodeLabel";
import { useSelectionState } from "@/components/Runs/runView/useSelectionState";

const iso = (ms: number) => new Date(Date.UTC(2026, 0, 1) + ms).toISOString();
const a: RunAttempt = { id: "a", started_at: iso(0), workflow_hash: "old" };
const b: RunAttempt = { id: "b", started_at: iso(100000), workflow_hash: "new" };
function snapshot(attempts: RunAttempt[]) {
  return { run: { id: "r", status: "failed_resumable", attempts, active_duration_ms: 20000 } as RunHeader, executions: [], last_seq: 3 } as RunSnapshot;
}
afterEach(() => { cleanup(); runStore.getState().reset(); });

// Mutation: keep the old header anchor/base after the live resume event.
it("advances duration from the resume clock before the next snapshot", () => {
  runStore.getState().applySnapshot(snapshot([a]));
  runStore.getState().applyEventsBatch([
    { run_id: "r", seq: 4, type: "run_resumed", timestamp: iso(100000), active_ms: 20000, data: { attempt: b } },
    { run_id: "r", seq: 5, type: "node_started", timestamp: iso(100010), active_ms: 20010, node_id: "work", data: { iteration: 0 } },
  ] as RunEvent[]);
  const { result } = renderHook(() => useRunMetrics(Date.parse(iso(105000))));
  expect(result.current.durationMs).toBe(5000);
});

// Mutation: remove attemptKey from the selection reset effect. A historical
// scrub then clips every execution on return to the current attempt.
it("clears scrub and node picks when switching attempts", () => {
  const { result, rerender } = renderHook(({ key }) => useSelectionState("r", key), { initialProps: { key: "a" } });
  act(() => { result.current.setScrubSeq(1); result.current.handleSelectNode("old"); });
  rerender({ key: "b" });
  expect(result.current.scrubSeq).toBeNull();
  expect(result.current.manualSelectedNodeId).toBeNull();
});

// Mutation: omit the attempt key in useNodeLabel's workflow query.
it("uses the selected attempt's authored labels", () => {
  runStore.getState().setRunId("r");
  runStore.getState().applySnapshot(snapshot([a, b]));
  runStore.getState().setSelectedAttemptID("a");
  const client = new QueryClient();
  client.setQueryData(["run-workflow", "r"], { nodes: [{ id: "work", description: "Changed" }] });
  client.setQueryData(["run-workflow", "r", "a"], { nodes: [{ id: "work", description: "Original" }] });
  const { result } = renderHook(() => useNodeLabel(), { wrapper: ({ children }) => createElement(QueryClientProvider, { client }, children) });
  expect(result.current("work")).toBe("Original");
});

// Mutation: extrapolate an absent monotonic clock from the last tool event.
it("counts from resume when the stream has no active_ms clock", () => {
  runStore.getState().applySnapshot(snapshot([a]));
  runStore.getState().applyEventsBatch([
    { run_id: "r", seq: 4, type: "run_resumed", timestamp: iso(100000), data: { attempt: b } },
    { run_id: "r", seq: 5, type: "tool_called", timestamp: iso(103000), node_id: "work", data: {} },
  ] as RunEvent[]);
  const { result } = renderHook(() => useRunMetrics(Date.parse(iso(105000))));
  expect(result.current.durationMs).toBe(5000);
});

// Mutation: use metrics.durationMs directly as the cumulative budget meter.
it("keeps cumulative duration spend in the current attempt budget meter", () => {
  const snap = snapshot([a, b]);
  snap.run.status = "finished";
  snap.run.budget = { max_duration: "60s" };
  snap.run.checkpoint = { budget_elapsed_ns: 55000000000 } as NonNullable<RunHeader["checkpoint"]>;
  runStore.getState().applySnapshot(snap);
  const metrics = { costUsd: 0, totalTokens: 0, durationMs: 5000 } as import("@/hooks/useRunMetrics").RunMetrics;
  const { container } = render(<BudgetSection run={snap.run} metrics={metrics} />);
  expect(container.textContent).toContain("55s");
});

// Mutation: ignore historical in useInlineSubbotData. The cached current
// child graph then changes an older parent attempt's graph.
it("keeps historical child graphs compact instead of inlining current source", () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["run-workflow", "child"], { nodes: [{ id: "new_node" }], edges: [], name: "new" });
  const children = new Map([["sub", [{ id: "child", status: "finished" } as import("@/api/runs").RunSummary]]]);
  const { result } = renderHook(() => useInlineSubbotData(children, new Map(), true), { wrapper: ({ children }) => createElement(QueryClientProvider, { client }, children) });
  expect(result.current.childWorkflowsByNode.size).toBe(0);
  expect(result.current.childExecutionsByRun.size).toBe(0);
});
