import { useMemo } from "react";

import type { RunEvent, ExecutionState } from "@/api/runs";
import { useRunStore } from "@/store/run";
import { useRunAttempt } from "./useRunAttempt";

// Metrics that depend on the events stream + executions map. Refreshing
// these is O(events) so we want to recompute only when one of those
// inputs actually changes. Duration ticks live separately.
export interface EventDrivenMetrics {
  costUsd: number;
  // Best-effort token total. claude_code reports only an aggregate
  // (via delegate_finished.data.tokens + node_finished.data.output._tokens),
  // claw reports per-step input/output via llm_step_finished. The
  // total is the sum of whatever each backend supplied; the chip
  // also shows the in/out split when both fields are non-zero.
  totalTokens: number;
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
  // Most recent budget_warning fields, when the runtime has fired at
  // least one. The backend warns once per dimension at the 80%
  // threshold; the latest message is the one worth surfacing because
  // the user already saw the earlier dimensions.
  budgetWarning: BudgetWarning | null;
  // True once a budget_exceeded event has been seen — the run will
  // fail (or has failed) hitting a hard cap.
  budgetExceeded: boolean;
  // Wall-clock ms of the most recent event the backend persisted (any
  // type). When the backend stops emitting — host loses network,
  // subprocess hangs — the gap to now() grows past expected cadence
  // and the UI can surface a "stalled" badge instead of the silent
  // freeze the 2026-05-21 internet outage produced. Null when no
  // events have arrived yet.
  lastEventAtMs: number | null;
}

export interface BudgetWarning {
  dimension: string;
  used: number;
  limit: number;
  // 0..1; UI multiplies by 100 for "%". Computed defensively from
  // used/limit when the event doesn't carry a ratio.
  ratio: number;
}

export interface RunMetrics extends EventDrivenMetrics {
  durationMs: number;
  isRunning: boolean;
}

export function useEventDrivenMetrics(): EventDrivenMetrics {
  const { events, executionsById } = useRunAttempt();

  return useMemo(() => computeEventDrivenMetrics(events, executionsById), [events, executionsById]);
}

export function computeEventDrivenMetrics(events: RunEvent[], executionsById: Map<string, ExecutionState>): EventDrivenMetrics {
    const m: EventDrivenMetrics = {
      costUsd: 0,
      totalTokens: 0,
      inputTokens: 0,
      outputTokens: 0,
      llmStepCount: 0,
      branchCountActive: 0,
      nodeCount: executionsById.size,
      failedCount: 0,
      pausedCount: 0,
      firstFailedNodeId: null,
      budgetWarning: null,
      budgetExceeded: false,
      lastEventAtMs: null,
    };

    for (const e of events) {
      if (e.timestamp) {
        const t = Date.parse(e.timestamp);
        if (Number.isFinite(t) && (m.lastEventAtMs === null || t > m.lastEventAtMs)) {
          m.lastEventAtMs = t;
        }
      }
      if (e.type === "llm_step_finished" && e.data) {
        m.llmStepCount += 1;
        // Per-step input/output split is unique to claw — kept for the
        // detailed chip tooltip. The TOTAL counter comes from
        // node_finished._tokens below to avoid double-counting on claw
        // (which emits both per-step and per-node totals).
        const inT = e.data["input_tokens"];
        if (typeof inT === "number") m.inputTokens += inT;
        const outT = e.data["output_tokens"];
        if (typeof outT === "number") m.outputTokens += outT;
      }
      // Cost + tokens are annotated per-node by the backend
      // (cost.Annotate writes _cost_usd / _tokens onto the node
      // output, which the runtime mirrors into node_finished.data.output).
      // claude_code is the dominant case today: it reports a single
      // aggregate token total per node, no in/out split, so the chip
      // falls back to total when both per-direction values are zero.
      // We also tolerate the legacy flat layout (data._cost_usd) so
      // older runs without the nested output field still report.
      if (e.type === "node_finished" && e.data) {
        const output = e.data["output"] as Record<string, unknown> | undefined;
        const flatCost = e.data["_cost_usd"];
        if (typeof flatCost === "number") {
          m.costUsd += flatCost;
        } else if (typeof output?.["_cost_usd"] === "number") {
          m.costUsd += output["_cost_usd"] as number;
        }
        // Aggregate token total. claude_code reports a single number
        // (no in/out split); claw could fold its in/out tally into
        // _tokens too. We don't double-count llm_step_finished here:
        // the per-step + per-node paths surface different backends so
        // exactly one will be populated for any given step.
        if (typeof output?.["_tokens"] === "number") {
          m.totalTokens += output["_tokens"] as number;
        }
      }
      if (e.type === "budget_warning" && e.data) {
        const dim = pickString(e.data, "dimension");
        const used = pickNumber(e.data, "used");
        const limit = pickNumber(e.data, "limit");
        const ratio =
          pickNumber(e.data, "ratio") ??
          (used != null && limit != null && limit > 0 ? used / limit : null);
        if (dim && ratio != null) {
          m.budgetWarning = {
            dimension: dim,
            used: used ?? 0,
            limit: limit ?? 0,
            ratio,
          };
        }
      }
      if (e.type === "budget_exceeded") {
        m.budgetExceeded = true;
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
}

function pickString(data: Record<string, unknown>, key: string): string | null {
  const v = data[key];
  return typeof v === "string" ? v : null;
}

function pickNumber(data: Record<string, unknown>, key: string): number | null {
  const v = data[key];
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

// useRunMetrics composes the event-driven metrics with a duration
// ticker. The ticker lives outside the events memo so the per-second
// re-render doesn't refold the entire events stream.
export function useRunMetrics(nowMs: number): RunMetrics {
  const snapshot = useRunStore((s) => s.snapshot);
  const scope = useRunAttempt();
  const events = useEventDrivenMetrics();

  // Parse the anchor only when the WS pushes a new value, not on every
  // ticker fire — re-parsing the same RFC3339 string at 1Hz is wasted
  // work and also causes the duration memo below to recompute even when
  // an unrelated snapshot field changed.
  const anchorMs = useMemo(() => {
    const iso = snapshot?.run.current_run_start;
    if (!iso) return null;
    const ms = new Date(iso).getTime();
    return Number.isFinite(ms) ? ms : null;
  }, [snapshot?.run.current_run_start]);

  const base = snapshot?.run.active_duration_ms ?? 0;

  const durationMs = useMemo(() => {
    if (scope.start) {
      const first = scope.events[0];
      const last = scope.events[scope.events.length - 1];
      const running = scope.isCurrent && snapshot?.run.status === "running";
      const baseActive = first?.active_ms ?? 0;
      // Store-authored observations (e.g. child attachment) do not carry
      // the engine clock. Use the last clock sample, not an absent tail field.
      let clock = last;
      for (let i = scope.events.length - 1; i >= 0; i--) {
        if ((scope.events[i]?.active_ms ?? 0) > 0) { clock = scope.events[i]; break; }
      }
      let totalActive = clock?.active_ms ?? 0;
      if (running && totalActive > 0) {
        // The snapshot can predate a resume. Event clocks remain usable until
        // a fresh header arrives; a header anchor must belong to this attempt.
        const eventClock = totalActive + Math.max(0, nowMs - Date.parse(clock?.timestamp ?? scope.start));
        const headerClock = anchorMs !== null && anchorMs >= Date.parse(scope.start)
          ? base + Math.max(0, nowMs - anchorMs) : 0;
        totalActive = Math.max(eventClock, headerClock);
      }
      if (totalActive > 0) return Math.max(0, totalActive - baseActive);
      const endMs = running ? nowMs : Date.parse(last?.timestamp ?? scope.start);
      return Math.max(0, endMs - Date.parse(scope.start));
    }
    if (anchorMs === null) return base;
    // Math.max guards against backwards client/server clock skew —
    // without the clamp, a skewed client could see the timer tick down.
    return base + Math.max(0, nowMs - anchorMs);
  }, [base, anchorMs, nowMs, scope, snapshot?.run.status]);

  return {
    ...events,
    durationMs,
    isRunning: scope.isCurrent && snapshot?.run.status === "running",
  };
}

// Re-export type for components consuming the events array directly.
export type { RunEvent };
