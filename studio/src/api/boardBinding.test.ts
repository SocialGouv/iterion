import { describe, expect, it } from "vitest";

import { formatStatusMap, parseStatusMap, type BoardBinding } from "./boardBinding";

// The status-map field is the operator's escape hatch from the shipped column
// vocabulary. It is parsed in the browser AND on the server; the browser half
// exists to fail fast with a readable message, so it has to be strict about
// exactly the shapes the server refuses — a pair silently dropped here would
// leave that column unmapped and inert, which looks like a working binding
// until someone notices a column that never syncs.

describe("parseStatusMap", () => {
  it("parses pairs, including a column name with a space", () => {
    const { map, error } = parseStatusMap("Todo=ready,In Progress=in_progress,Shipped=done");
    expect(error).toBeUndefined();
    expect(map).toEqual({
      Todo: "ready",
      "In Progress": "in_progress",
      Shipped: "done",
    });
  });

  it("trims around the separators", () => {
    const { map } = parseStatusMap("  Todo = ready , Shipped=done ");
    expect(map).toEqual({ Todo: "ready", Shipped: "done" });
  });

  it("treats an empty field as 'no override', not an empty map", () => {
    expect(parseStatusMap("   ")).toEqual({});
  });

  it.each(["Todo", "Todo=", "=ready", "Todo=ready,,", "Todo=ready=oops"])(
    "refuses the malformed input %j",
    (input) => {
      expect(parseStatusMap(input).error).toBeTruthy();
    },
  );

  it("refuses a column named twice, and says which", () => {
    const { error } = parseStatusMap("Todo=ready,Todo=done");
    expect(error).toContain("Todo");
  });
});

describe("formatStatusMap", () => {
  it("round-trips a binding's effective map back into the field", () => {
    const b = {
      status_mapping: [
        { status: "Planned", state: "ready" },
        { status: "Done", state: "done" },
      ],
    } as BoardBinding;
    const rendered = formatStatusMap(b);
    expect(rendered).toBe("Planned=ready,Done=done");
    expect(parseStatusMap(rendered).map).toEqual({ Planned: "ready", Done: "done" });
  });

  it("renders nothing for a binding with no stored map", () => {
    expect(formatStatusMap(null)).toBe("");
    expect(formatStatusMap({} as BoardBinding)).toBe("");
  });
});
