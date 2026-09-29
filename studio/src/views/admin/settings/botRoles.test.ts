import { describe, expect, it } from "vitest";

import { storedRole } from "./botRoles";
import type { BotRolesSettingsView } from "@/api/adminSettings";

const base: BotRolesSettingsView = {
  effective: { reviewer: "review-pr", revi_converse: "revi-converse", brancher: "branch-improve-loop", implementer: "feature-dev" },
  origin: "default",
};

describe("storedRole", () => {
  it("returns null with no override record", () => {
    expect(storedRole(base, "reviewer")).toBeNull();
    expect(storedRole(undefined, "reviewer")).toBeNull();
  });
  it("reads a set override and treats an empty/absent field as inherit", () => {
    const view: BotRolesSettingsView = {
      ...base,
      origin: "db",
      stored: { reviewer: "my-reviewer", implementer: "", updated_at: "2026-09-18T00:00:00Z" },
    };
    expect(storedRole(view, "reviewer")).toBe("my-reviewer");
    expect(storedRole(view, "implementer")).toBeNull();
    expect(storedRole(view, "brancher")).toBeNull();
  });
});
