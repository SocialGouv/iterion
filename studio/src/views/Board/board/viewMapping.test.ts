import { describe, expect, it } from "vitest";

import type { FilterState } from "./viewMapping";
import { filtersFromView, viewFromFilters } from "./viewMapping";

describe("view <-> filter-state mapping", () => {
  it("snapshots the bot scope into a saved view", () => {
    const state: FilterState = {
      search: "",
      labels: [],
      assignee: "",
      bot: "feature-dev",
      sort: "priority",
      group: "none",
    };
    const v = viewFromFilters("Featurly pipeline", state);
    expect(v.bot).toBe("feature-dev");
  });

  it("drops an empty bot scope (omitted from the persisted view)", () => {
    const state: FilterState = {
      search: "",
      labels: [],
      assignee: "",
      bot: "",
      sort: "priority",
      group: "none",
    };
    expect(viewFromFilters("All", state).bot).toBeUndefined();
  });

  it("restores the bot scope when a saved view is applied", () => {
    expect(filtersFromView({ name: "x", bot: "docs-refresh" }).bot).toBe(
      "docs-refresh",
    );
    expect(filtersFromView({ name: "x" }).bot).toBe("");
  });

  it("round-trips a full filter combo preserving bot", () => {
    const state: FilterState = {
      search: "auth",
      labels: ["bug", "backend"],
      assignee: "jo",
      bot: "feature-dev",
      sort: "created",
      group: "assignee",
    };
    const restored = filtersFromView(viewFromFilters("Combo", state));
    expect(restored).toEqual(state);
  });
});
