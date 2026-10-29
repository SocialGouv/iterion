import { describe, expect, it } from "vitest";

import type { UnitInfo } from "@/api/types";
import { SOURCE_MERGED } from "@/store/document";

import { resolveProposalTarget } from "./proposalTarget";

const unit: UnitInfo = {
  root: "bots/demo",
  main: "main.bot",
  revision: "r1",
  files: [{ rel: "main.bot", imports: ["lib/nodes.bot"] }, { rel: "lib/nodes.bot" }],
};

describe("resolveProposalTarget", () => {
  it("targets the Source view picker's selection, never the hint's file", () => {
    expect(resolveProposalTarget(unit, "lib/nodes.bot", null)).toEqual({ rel: "lib/nodes.bot" });
    // The hint MATCHING the picker applies — the selection was already the target.
    expect(resolveProposalTarget(unit, "lib/nodes.bot", "lib/nodes.bot")).toEqual({ rel: "lib/nodes.bot" });
  });

  it("defaults to the main, as the picker does before it first renders", () => {
    expect(resolveProposalTarget(unit, null, null)).toEqual({ rel: "main.bot" });
    // A selection whose file left the unit falls back the same way.
    expect(resolveProposalTarget(unit, "lib/gone.bot", null)).toEqual({ rel: "main.bot" });
  });

  it("targets nothing while the picker is on the merged program", () => {
    const decision = resolveProposalTarget(unit, SOURCE_MERGED, null);
    expect("reason" in decision && decision.reason).toMatch(/merged program/i);
  });

  it("disables on a hint/picker mismatch rather than redirecting the write", () => {
    // The doctrine case: a proposal carrying main.bot's text while NAMING
    // lib/nodes.bot must never overwrite lib/nodes.bot (#1647).
    const decision = resolveProposalTarget(unit, "main.bot", "lib/nodes.bot");
    expect("reason" in decision && decision.reason).toMatch(/switch the picker to lib\/nodes\.bot/i);
  });

  it("says aloud a hint naming a file the bot does not have", () => {
    const decision = resolveProposalTarget(unit, "main.bot", "lib/hallucinated.bot");
    expect("reason" in decision && decision.reason).toMatch(/not one of this bot's files/i);
  });
});
