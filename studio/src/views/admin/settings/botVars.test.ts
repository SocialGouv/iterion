import { describe, expect, it } from "vitest";

import { buildBotVarsPatch, newRow, rowsFromVars } from "./botVars";

describe("rowsFromVars", () => {
  it("builds sorted rows from the stored map", () => {
    const rows = rowsFromVars({ ITERION_B: "2", ITERION_A: "1" });
    expect(rows.map((r) => [r.key, r.value])).toEqual([
      ["ITERION_A", "1"],
      ["ITERION_B", "2"],
    ]);
  });
  it("is empty for no stored vars", () => {
    expect(rowsFromVars(undefined)).toEqual([]);
  });
});

describe("buildBotVarsPatch", () => {
  const stored = { ITERION_A: "1", ITERION_B: "2" };

  it("is empty when nothing changed", () => {
    const rows = [newRow("ITERION_A", "1"), newRow("ITERION_B", "2")];
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({});
  });

  it("sets a changed value and adds a new key", () => {
    const rows = [newRow("ITERION_A", "9"), newRow("ITERION_B", "2"), newRow("ITERION_C", "3")];
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({ ITERION_A: "9", ITERION_C: "3" });
  });

  it("clears a removed key with null", () => {
    const rows = [newRow("ITERION_A", "1")]; // B dropped
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({ ITERION_B: null });
  });

  it("ignores an empty-key row", () => {
    const rows = [newRow("ITERION_A", "1"), newRow("ITERION_B", "2"), newRow("", "x")];
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({});
  });

  it("flags a duplicate key rather than silently collapsing", () => {
    const rows = [newRow("ITERION_A", "1"), newRow("ITERION_A", "2")];
    const { error } = buildBotVarsPatch(rows, stored);
    expect(error).toMatch(/duplicate key/);
  });

  it("trims the key before diffing", () => {
    const rows = [newRow("  ITERION_A  ", "1"), newRow("ITERION_B", "2")];
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({});
  });
});
