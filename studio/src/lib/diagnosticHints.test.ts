import { describe, expect, it } from "vitest";
import { getHint } from "./diagnosticHints";

describe("getHint", () => {
  it("returns the registered hint for a known code", () => {
    const h = getHint("C001");
    expect(h.title).toBe("Edge references unknown node");
    expect(h.docsAnchor).toBe("c001");
  });

  it("matches case-insensitively", () => {
    const upper = getHint("C043");
    const lower = getHint("c043");
    expect(lower).toEqual(upper);
    expect(lower.title).toBe("Invalid compaction values");
  });

  it("falls back to a generic record for unknown codes", () => {
    const h = getHint("C999");
    expect(h.title).toBe("Diagnostic C999");
    expect(h.hint).toMatch(/no hint/i);
    expect(h.docsAnchor).toBeUndefined();
  });

  it("documents the four newer compile diagnostics (C039–C042)", () => {
    expect(getHint("C039").title).toMatch(/compute/i);
    expect(getHint("C040").title).toMatch(/expression/i);
    expect(getHint("C041").title).toMatch(/duplicate/i);
    expect(getHint("C042").title).toMatch(/reserved/i);
  });

  it("uses 'model or backend' wording for C018 (delegate keyword removed)", () => {
    const h = getHint("C018");
    expect(h.title).toBe("Missing model or backend");
    expect(h.hint).toMatch(/backend/);
    expect(h.hint).not.toMatch(/delegate/);
  });

  it("tells a C011 author the order that works, not only to drop an edge", () => {
    // The guarded loop-exhaustion exit shares the back-edge's condition and
    // compiles when the back-edge is declared first AND the exit leaves the
    // loop; a hint that only says "use distinct conditions" sends that author
    // the wrong way, and one naming half the permission sends him into the
    // refusal the other half names.
    const h = getHint("C011");
    expect(h.hint).toMatch(/back-edge/);
    expect(h.hint).toMatch(/declare the back-edge first/);
    expect(h.hint).toMatch(/route the exit out of the iteration/);
  });

  it("does not surface the obsolete C029 hint anymore", () => {
    // C029 (interaction-on-non-delegate) was removed when the parser
    // dropped the `delegate:` keyword. Should fall through to the
    // generic placeholder, not return a stale 'delegate'-flavoured hint.
    const h = getHint("C029");
    expect(h.title).toBe("Diagnostic C029");
    expect(h.hint).not.toMatch(/delegate/);
  });
});
