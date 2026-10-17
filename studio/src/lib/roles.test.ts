import { describe, expect, it } from "vitest";

import { ORG_ROLES, outranksViewer } from "./roles";

// The ladder rule the server enforces on every roster write: a member ranked
// above its author is not reached, and equal rank is.
describe("outranksViewer", () => {
  it("places a row above a viewer of lower rank", () => {
    expect(outranksViewer("owner", "admin", ORG_ROLES)).toBe(true);
    expect(outranksViewer("owner", "admin")).toBe(true);
    expect(outranksViewer("admin", "member", ORG_ROLES)).toBe(true);
  });

  it("passes equal rank, and any rank below", () => {
    expect(outranksViewer("admin", "admin", ORG_ROLES)).toBe(false);
    expect(outranksViewer("owner", "owner", ORG_ROLES)).toBe(false);
    expect(outranksViewer("member", "owner", ORG_ROLES)).toBe(false);
    expect(outranksViewer("viewer", "admin")).toBe(false);
  });

  it("is outranked by every rung when the viewer holds no role", () => {
    expect(outranksViewer("member", null, ORG_ROLES)).toBe(true);
    expect(outranksViewer("viewer", null)).toBe(true);
  });

  it("lets the capability off the ladder through, and covers no rank the ladder does not carry", () => {
    expect(outranksViewer("config_editor", "admin")).toBe(false);
    expect(outranksViewer("config_editor", null)).toBe(false);
    expect(outranksViewer("archon", "owner", ORG_ROLES)).toBe(true);
  });

  it("places the off-ladder capability only on the ladder that carries it", () => {
    // On the ORG ladder `config_editor` is a value no rung carries:
    // identity.OrgRole ranks it 0 and AtLeast refuses it, so the server
    // refuses such a row exactly as it refuses `archon`. Reading the
    // exception off the STRING answered "outranks nothing" there and enabled
    // a control the server always refuses.
    expect(outranksViewer("config_editor", "owner", ORG_ROLES)).toBe(true);
    expect(outranksViewer("config_editor", "member", ORG_ROLES)).toBe(true);
    expect(outranksViewer("config_editor", null, ORG_ROLES)).toBe(true);
  });
});
