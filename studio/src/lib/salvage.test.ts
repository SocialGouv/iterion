import { describe, expect, it } from "vitest";

import { salvageRefusal } from "./salvage";

// A refusal has to name a way OUT that exists, and one that works all the
// way to the file. Two shapes were tried for a bot in several files and
// both named a CONTROL that fails: the "files drawer", which renders only
// for a cloud `botsource://` path; then the Source view, whose Apply
// answers 200 and whose Save then refuses. The control that works differs
// per twin and on the cloud twin there is none. So the message names the
// CONSTRAINT instead — one fact, true on both twins, witnessed server-side
// by TestASalvagedUnitIsRepairedWhereTheSaveReadsIt.
describe("salvageRefusal", () => {
  it("says nothing when the document is the program", () => {
    expect(salvageRefusal({ salvaged: false })).toBeNull();
    expect(salvageRefusal({ salvaged: false, unit: { main: "main.bot" } })).toBeNull();
  });

  it("points a single file at the Source view, where Edit and Apply exist", () => {
    const refusal = salvageRefusal({ salvaged: true, unit: null });
    expect(refusal).toMatch(/Source view/i);
  });

  it("tells a bot in several files the constraint, not a control", () => {
    const refusal = salvageRefusal({ salvaged: true, unit: { main: "main.bot" } });
    expect(refusal).not.toBeNull();
    // The fact the save enforces, said as a fact.
    expect(refusal).toMatch(/where this bot's files live/i);
    expect(refusal).toMatch(/nothing edited here can reach/i);
    // None of the three controls that were named and did not work: the
    // drawer (cloud-only), the Source view / Apply (200 then 422), and
    // "on disk", which is not where a cloud bundle's files live.
    expect(refusal).not.toMatch(/files drawer/i);
    expect(refusal).not.toMatch(/Source view/i);
    expect(refusal).not.toMatch(/Apply/);
    expect(refusal).not.toMatch(/on disk/i);
  });
});
