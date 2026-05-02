import { useMemo } from "react";

import type { RunEvent } from "@/api/runs";
import { useRunStore } from "@/store/run";

// CostBucket is one row in any of the report's three breakdowns
// (provider / model / node). Counts and totals are rolled up from
// node_finished events; see useRunReport for the source of truth.
export interface CostBucket {
  // The grouping key — provider name, full model id, or node_id.
  key: string;
  // What to show in the UI — sometimes equal to key (provider), sometimes
  // a friendlier rendering (e.g. node label without branch noise).
  label: string;
  costUsd: number;
  tokens: number;
  // How many node_finished rows fed this bucket. For nodes this is the
  // number of executions (≥1 for loops/retries); for providers/models
  // it's the number of node executions that picked them.
  count: number;
}

export interface RunReport {
  totalCostUsd: number;
  totalTokens: number;
  // Aggregations sorted by costUsd descending. Empty when no
  // node_finished events have arrived yet.
  byProvider: CostBucket[];
  byModel: CostBucket[];
  byNode: CostBucket[];
  // True when at least one node_finished event surfaced a cost. Lets
  // the UI render a clear empty-state instead of an all-zero report
  // for runs that never used an LLM (pure tool workflows).
  hasCost: boolean;
}

// providerOf derives the API-key-level grouping from the (model, backend)
// pair the runtime annotates onto each node_finished. The rules mirror
// pkg/backend/cost: claw model strings carry a "provider/model" prefix,
// while claude_code and codex bypass that and produce bare model ids.
function providerOf(model: string, backend: string): string {
  if (model.includes("/")) {
    const head = model.split("/", 1)[0]?.toLowerCase().trim();
    if (head) return head;
  }
  const b = backend.toLowerCase();
  if (b === "claude_code") return "anthropic";
  if (b === "codex") return "openai";
  return b || "unknown";
}

// modelLabel normalizes the display string. claw-format ids are kept
// as-is ("anthropic/claude-sonnet-4-6") so the user can distinguish
// providers at a glance; bare ids ("claude-opus-4-7") get the inferred
// provider prefixed for symmetry in the by-model breakdown.
function modelLabel(model: string, provider: string): string {
  if (!model) return "(unknown)";
  if (model.includes("/")) return model;
  return `${provider}/${model}`;
}

// useRunReport folds the in-memory event stream into the three cost
// breakdowns the Report tab renders. Source of truth is node_finished
// (cost.Annotate runs once per node and the runtime mirrors _cost_usd
// /_tokens onto the event). Per-LLM-step cost is not emitted, so a node
// that mixes models within one execution is attributed to the dominant
// model of its node_finished output — accurate for ~95% of workflows.
export function useRunReport(): RunReport {
  const events = useRunStore((s) => s.events);
  return useMemo(() => buildRunReport(events), [events]);
}

// buildRunReport is the pure function the hook wraps. Exported for
// testing without a Zustand store.
export function buildRunReport(events: RunEvent[]): RunReport {
  // Maps for stable per-key accumulation; finalised into sorted arrays.
  const byProvider = new Map<string, CostBucket>();
  const byModel = new Map<string, CostBucket>();
  const byNode = new Map<string, CostBucket>();
  let totalCostUsd = 0;
  let totalTokens = 0;
  let hasCost = false;

  for (const e of events) {
    if (e.type !== "node_finished" || !e.data) continue;
    const cost = numberOr(e.data["_cost_usd"], 0);
    const tokens = numberOr(e.data["_tokens"], 0);
    if (cost === 0 && tokens === 0) continue;
    if (cost > 0) hasCost = true;
    totalCostUsd += cost;
    totalTokens += tokens;

    const out = (e.data["output"] ?? {}) as Record<string, unknown>;
    const model = stringOr(out["_model"], "");
    const backend = stringOr(out["_backend"], "");
    const provider = providerOf(model, backend);

    bumpBucket(byProvider, provider, provider, cost, tokens);
    if (model) {
      const ml = modelLabel(model, provider);
      bumpBucket(byModel, ml, ml, cost, tokens);
    }

    const nodeId = e.node_id ?? "(unknown)";
    bumpBucket(byNode, nodeId, nodeId, cost, tokens);
  }

  return {
    totalCostUsd,
    totalTokens,
    hasCost,
    byProvider: sortByCost(byProvider),
    byModel: sortByCost(byModel),
    byNode: sortByCost(byNode),
  };
}

function bumpBucket(
  m: Map<string, CostBucket>,
  key: string,
  label: string,
  cost: number,
  tokens: number,
) {
  const cur = m.get(key);
  if (cur) {
    cur.costUsd += cost;
    cur.tokens += tokens;
    cur.count += 1;
    return;
  }
  m.set(key, { key, label, costUsd: cost, tokens, count: 1 });
}

function sortByCost(m: Map<string, CostBucket>): CostBucket[] {
  return Array.from(m.values()).sort((a, b) => b.costUsd - a.costUsd);
}

function numberOr(v: unknown, fallback: number): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}

function stringOr(v: unknown, fallback: string): string {
  return typeof v === "string" ? v : fallback;
}
