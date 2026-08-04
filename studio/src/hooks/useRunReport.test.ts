import { describe, expect, it } from "vitest";

import type { RunEvent } from "@/api/runs";
import { buildRunReport } from "./useRunReport";

function nodeFinished(
  nodeId: string,
  data: Record<string, unknown>,
): RunEvent {
  return { type: "node_finished", node_id: nodeId, data } as RunEvent;
}

describe("buildRunReport", () => {
  it("reports no usage for an event stream without LLM nodes", () => {
    const r = buildRunReport([
      nodeFinished("plan", { output: {} }),
      { type: "run_started" } as RunEvent,
    ]);
    expect(r.hasUsage).toBe(false);
    expect(r.hasCost).toBe(false);
  });

  it("aggregates cost and tokens per provider/model/node", () => {
    const r = buildRunReport([
      nodeFinished("synthesize", {
        _cost_usd: 0.5,
        _tokens: 1000,
        output: { _model: "claude-opus-5", _backend: "claude_code" },
      }),
      nodeFinished("judge", {
        _cost_usd: 0.25,
        _tokens: 400,
        output: { _model: "openai/gpt-5.5", _backend: "claw" },
      }),
    ]);
    expect(r.hasCost).toBe(true);
    expect(r.hasUsage).toBe(true);
    expect(r.totalCostUsd).toBeCloseTo(0.75);
    expect(r.totalTokens).toBe(1400);
    expect(r.byProvider.map((b) => b.key)).toEqual(["anthropic", "openai"]);
    expect(r.byNode[0]?.key).toBe("synthesize");
  });

  it("keeps a tokens-only run reportable (unpriced model: no _cost_usd)", () => {
    // The exact shape of the feed-watch cloud runs: claude_code with no
    // node-declared model annotated tokens but no cost.
    const r = buildRunReport([
      nodeFinished("synthesize", {
        _tokens: 5995,
        output: { _model: "", _backend: "claude_code" },
      }),
    ]);
    expect(r.hasCost).toBe(false);
    expect(r.hasUsage).toBe(true);
    expect(r.totalTokens).toBe(5995);
    expect(r.byProvider.map((b) => b.key)).toEqual(["anthropic"]);
    expect(r.byNode[0]?.tokens).toBe(5995);
  });

  it("ranks equal-cost buckets by tokens", () => {
    const r = buildRunReport([
      nodeFinished("small", { _tokens: 100, output: {} }),
      nodeFinished("big", { _tokens: 900, output: {} }),
    ]);
    expect(r.byNode.map((b) => b.key)).toEqual(["big", "small"]);
  });
});
