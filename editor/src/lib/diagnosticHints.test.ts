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

  it("does not surface the obsolete C029 hint anymore", () => {
    // C029 (interaction-on-non-delegate) was removed when the parser
    // dropped the `delegate:` keyword. Should fall through to the
    // generic placeholder, not return a stale 'delegate'-flavoured hint.
    const h = getHint("C029");
    expect(h.title).toBe("Diagnostic C029");
    expect(h.hint).not.toMatch(/delegate/);
  });
});
