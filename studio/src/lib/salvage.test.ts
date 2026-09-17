import { describe, expect, it } from "vitest";

import { salvageRefusal } from "./salvage";

// A refusal has to name a way OUT that exists. The Source view is read-only
// for a bot in several files — its Edit/Apply controls are hidden whenever
// `unit` is set — so a unit's author told to "repair it in the Source view"
// is told to use a control the render gate removed, and the buffer has no
// route at all: Save, Save As, Download, Copy and Launch are all refused.
describe("salvageRefusal", () => {
  it("says nothing when the document is the program", () => {
    expect(salvageRefusal({ salvaged: false })).toBeNull();
    expect(salvageRefusal({ salvaged: false, unit: { main: "main.bot" } })).toBeNull();
  });

  it("points a single file at the Source view, where Edit and Apply exist", () => {
    const refusal = salvageRefusal({ salvaged: true, unit: null });
    expect(refusal).toMatch(/Source view/i);
  });

  it("never points a bot in several files at the Source view", () => {
    const refusal = salvageRefusal({ salvaged: true, unit: { main: "main.bot" } });
    expect(refusal).not.toBeNull();
    // The control it would name is hidden for a unit.
    expect(refusal).not.toMatch(/Source view/i);
    // And it names one that is there.
    expect(refusal).toMatch(/files drawer|on disk/i);
  });
});
