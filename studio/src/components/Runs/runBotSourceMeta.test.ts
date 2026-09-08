import { describe, expect, it } from "vitest";

import type { RunHeader, RunStatus } from "@/api/runs";

import { botSourceTierMeta } from "./runBotSourceMeta";

function mkRun(partial: Partial<RunHeader>): RunHeader {
  return {
    id: "run_x",
    workflow_name: "wf",
    status: (partial.status ?? "finished") as RunStatus,
    created_at: "2026-09-08T12:00:00Z",
    updated_at: "2026-09-08T12:00:00Z",
    active_duration_ms: 0,
    ...partial,
  } as RunHeader;
}

// #871 — the whole defect was an inert tier reading as a working one, so
// the rule this helper exists to hold is: an absent or unknown tier is
// NEVER rendered as a tier. Only a value the launcher actually recorded
// gets a label.
describe("botSourceTierMeta", () => {
  it("names the team fork, and says which team owns the row", () => {
    const m = botSourceTierMeta(
      mkRun({ bot_source_tier: "team", bot_source_tenant: "acme" }),
    );
    expect(m?.label).toBe("team bot");
    expect(m?.detail).toContain("acme");
    expect(m?.notable).toBe(true);
  });

  it("names a platform override", () => {
    const m = botSourceTierMeta(mkRun({ bot_source_tier: "platform" }));
    expect(m?.label).toBe("platform override");
    expect(m?.notable).toBe(true);
  });

  it("states `baked` positively, but never flags it as notable", () => {
    const m = botSourceTierMeta(mkRun({ bot_source_tier: "baked" }));
    expect(m?.label).toBe("baked catalog");
    expect(m?.notable).toBe(false);
  });

  it("returns null when no tier was recorded — absence is not `baked`", () => {
    expect(botSourceTierMeta(mkRun({}))).toBeNull();
    expect(botSourceTierMeta(mkRun({ bot_source_tier: "" }))).toBeNull();
    expect(botSourceTierMeta(mkRun({ bot_source_tier: "   " }))).toBeNull();
  });

  it("returns null for a tier this build does not know", () => {
    // A newer server naming a fourth tier must render nothing rather than
    // guess: a wrong claim about which bundle ran is worse than silence.
    expect(botSourceTierMeta(mkRun({ bot_source_tier: "marketplace" }))).toBeNull();
  });
});
