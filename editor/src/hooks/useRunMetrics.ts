import { useMemo } from "react";

import type { RunEvent } from "@/api/runs";
import { useRunStore } from "@/store/run";

// Metrics that depend on the events stream + executions map. Refreshing
// these is O(events) so we want to recompute only when one of those
// inputs actually changes. Duration ticks live separately.
export interface EventDrivenMetrics {
  costUsd: number;
  inputTokens: number;
  outputTokens: number;
  // Total LLM step events seen (proxy for "LLM rounds").
  llmStepCount: number;
  branchCountActive: number;
  nodeCount: number;
  failedCount: number;
  pausedCount: number;
  // First failed execution id (for "jump to failed" affordance).
  firstFailedNodeId: string | null;
}

export interface RunMetrics extends EventDrivenMetrics {
  durationMs: number;
  isRunning: boolean;
}

export function useEventDrivenMetrics(): EventDrivenMetrics {
  const events = useRunStore((s) => s.events);
  const executionsById = useRunStore((s) => s.executionsById);

  return useMemo<EventDrivenMetrics>(() => {
    const m: EventDrivenMetrics = {
      costUsd: 0,
      inputTokens: 0,
      outputTokens: 0,
      llmStepCount: 0,
      branchCountActive: 0,
      nodeCount: executionsById.size,
      failedCount: 0,
      pausedCount: 0,
      firstFailedNodeId: null,
    };

    for (const e of events) {
      if (e.type === "llm_step_finished" && e.data) {
        m.llmStepCount += 1;
        const inT = e.data["input_tokens"];
        if (typeof inT === "number") m.inputTokens += inT;
        const outT = e.data["output_tokens"];
        if (typeof outT === "number") m.outputTokens += outT;
      }
      // Cost is annotated per-node by the backend (cost.Annotate writes
      // _cost_usd onto the node output, which the runtime mirrors into
      // node_finished.data). LLMStepInfo carries no cost, so summing
      // cost from llm_step_finished would always yield $0.
      if (e.type === "node_finished" && e.data) {
        const c = e.data["_cost_usd"];
        if (typeof c === "number") m.costUsd += c;
      }
    }

    for (const ex of executionsById.values()) {
      if (ex.status === "running") m.branchCountActive += 1;
      if (ex.status === "failed") {
        m.failedCount += 1;
        if (!m.firstFailedNodeId) m.firstFailedNodeId = ex.ir_node_id;
      }
      if (ex.status === "paused_waiting_human") m.pausedCount += 1;
    }

    return m;
  }, [events, executionsById]);
}

// useRunMetrics composes the event-driven metrics with a duration
// ticker. The ticker lives outside the events memo so the per-second
// re-render doesn't refold the entire events stream.
export function useRunMetrics(nowMs: number): RunMetrics {
  const snapshot = useRunStore((s) => s.snapshot);
  const events = useEventDrivenMetrics();

  const durationMs = useMemo(() => {
    if (!snapshot?.run.created_at) return 0;
    const start = new Date(snapshot.run.created_at).getTime();
    // Status is the source of truth for "is this run still going?". A
    // resumed run can briefly carry a stale finished_at if the backend
    // ran an older binary; gate on terminal status so the duration
    // doesn't freeze mid-run.
    const status = snapshot.run.status;
    const isTerminal =
      status === "finished" ||
      status === "failed" ||
      status === "failed_resumable" ||
      status === "cancelled";
    const end = isTerminal && snapshot.run.finished_at
      ? new Date(snapshot.run.finished_at).getTime()
      : nowMs;
    if (!Number.isFinite(start) || !Number.isFinite(end)) return 0;
    return Math.max(0, end - start);
  }, [snapshot, nowMs]);

  return {
    ...events,
    durationMs,
    isRunning: snapshot?.run.status === "running",
  };
}

// Re-export type for components consuming the events array directly.
export type { RunEvent };
