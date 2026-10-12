import { describe, expect, it } from "vitest";

import { ADMIN_ITEMS, ADMIN_PATHS, adminPath, consolePath } from "./adminRoutes";

describe("adminPath", () => {
  // What a link written OUTSIDE the console needs: the console's own base
  // applied once. Inside the console the table's paths are used as they are —
  // the nested router applies that base itself.
  it("prefixes a console path with the console's base", () => {
    expect(adminPath("/orgs")).toBe("/admin/orgs");
    expect(adminPath("/settings/bot-vars")).toBe("/admin/settings/bot-vars");
  });
});

describe("consolePath", () => {
  // The trailing-slash form is a published address (the Go legacy redirect
  // sends /admin/users/ to /studio/admin/users/), and wouter reports it as
  // "/users/" inside the nest while still matching the route "/users".
  it("credits a trailing-slash location to the path it renders", () => {
    expect(consolePath("/users")).toBe("/users");
    expect(consolePath("/users/")).toBe("/users");
    expect(consolePath("/settings/sandbox/")).toBe("/settings/sandbox");
  });

  // The bound, not just the example: wouter's matcher allows exactly one
  // optional trailing slash, so "/users//" matches NO route (measured). A
  // normaliser that stripped every slash would mark an entry current on a
  // URL that renders the catch-all redirect instead.
  it("stops at one slash, as the route matcher does, and leaves the root alone", () => {
    expect(consolePath("/users//")).toBe("/users/");
    expect(consolePath("/")).toBe("/");
  });

  // The other dimension the matcher is blind to: wouter compiles route
  // patterns with the `i` flag, so /admin/USERS renders the users console
  // (AdminConsoleRoutes.test.tsx measures it). Every table path is lowercase,
  // so folding case credits that render to its entry.
  it("credits a case variant to the path it renders", () => {
    expect(consolePath("/USERS")).toBe("/users");
    expect(consolePath("/Settings/Sandbox/")).toBe("/settings/sandbox");
  });
});

describe("the console's route table", () => {
  it("declares each path once, and one nav entry per path", () => {
    expect(new Set(ADMIN_PATHS).size).toBe(ADMIN_PATHS.length);
    expect(ADMIN_ITEMS).toHaveLength(ADMIN_PATHS.length);
  });

  it("labels every entry", () => {
    expect(ADMIN_ITEMS.filter((item) => item.label.trim() === "")).toEqual([]);
  });

  // consolePath folds case, which credits a case variant to its entry only
  // while the table itself is lowercase. A path that is not would be marked
  // current nowhere, so the precondition is a gate and not a comment.
  it("declares every path in lowercase, as consolePath's fold requires", () => {
    expect(ADMIN_PATHS.filter((path) => path !== path.toLowerCase())).toEqual([]);
  });
});
