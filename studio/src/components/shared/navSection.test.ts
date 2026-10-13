import { describe, expect, it } from "vitest";

import { deriveSection } from "./navSection";
import { ADMIN_BASE, adminPath } from "@/views/admin/adminRoutes";

// The sidebar's highlighted row comes from the location's FIRST segment, so
// every section that owns a route family has to be keyed by the segment that
// family actually uses. The console is the one family whose mount point is
// declared in code (ADMIN_BASE, which App.tsx mounts): a segment spelled out
// here would keep highlighting the old one after a move, with nothing to say so.
describe("deriveSection", () => {
  it("highlights the console on any of its screens, from the console's own base", () => {
    expect(deriveSection(ADMIN_BASE)).toBe("admin");
    expect(deriveSection(adminPath("/users"))).toBe("admin");
    expect(deriveSection(adminPath("/settings/bot-vars"))).toBe("admin");
  });

  it("highlights home at the root and nothing for an unmapped segment", () => {
    expect(deriveSection("/")).toBe("home");
    expect(deriveSection("/nowhere")).toBeUndefined();
  });

  // The first segment only, so a longer name that merely starts with a
  // section's own does not borrow its highlight.
  it("keys on the whole first segment", () => {
    expect(deriveSection("/runs/run-1")).toBe("runs");
    expect(deriveSection("/boardroom")).toBeUndefined();
  });
});
