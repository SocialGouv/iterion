import { describe, expect, it } from "vitest";

import { buildBotVarsPatch, newRow, refusedList, rowsFromVars } from "./botVars";

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

  // The merge contract: a blanked value must travel as null (clear), never as
  // "" — the server's entry rule 400s on a blank value instead of clearing.
  it("clears a blanked stored key with null, not an empty string", () => {
    const rows = [newRow("ITERION_A", ""), newRow("ITERION_B", "2")];
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({ ITERION_A: null });
  });

  it("treats a whitespace-only value as blanked (the server trims too)", () => {
    const rows = [newRow("ITERION_A", "   "), newRow("ITERION_B", "2")];
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({ ITERION_A: null });
  });

  it("drops a blanked key that was never stored (null there is refused)", () => {
    const rows = [
      newRow("ITERION_A", "1"),
      newRow("ITERION_B", "2"),
      newRow("ITERION_NEW", ""),
    ];
    expect(buildBotVarsPatch(rows, stored).patch).toEqual({});
  });
});

describe("refusedList", () => {
  it("flattens the refused map into a stable display order", () => {
    expect(refusedList({ ITERION_B: "bad value", ITERION_A: "bad name" })).toEqual([
      { name: "ITERION_A", reason: "bad name" },
      { name: "ITERION_B", reason: "bad value" },
    ]);
  });

  it("is empty when nothing is refused", () => {
    expect(refusedList(undefined)).toEqual([]);
    expect(refusedList({})).toEqual([]);
  });
});
