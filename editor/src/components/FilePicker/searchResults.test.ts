import { describe, expect, it } from "vitest";
import type { FileEntry } from "@/api/types";
import { buildSearchResults } from "./searchResults";

const file = (name: string, size = 0): FileEntry => ({ name, size });

describe("buildSearchResults", () => {
  it("returns [] for empty / whitespace-only query", () => {
    expect(buildSearchResults("", ["a.iter"], [file("b.iter")], ["c.iter"])).toEqual([]);
    expect(buildSearchResults("   ", ["a.iter"], [file("b.iter")], ["c.iter"])).toEqual([]);
  });

  it("matches across all three sources — the bug scenario", () => {
    // User has Recents active, types a substring that only exists in
    // Examples. Old code would return nothing; new code finds it.
    const results = buildSearchResults(
      "kanban",
      ["plan.iter"],
      [file("review.iter")],
      ["kanban_review.iter"],
    );
    expect(results).toEqual([{ kind: "example", name: "kanban_review.iter" }]);
  });

  it("aggregates matches across recents, files, and examples", () => {
    const results = buildSearchResults(
      "review",
      ["session_review.iter", "plan.iter"],
      [file("review.iter"), file("kanban.iter")],
      ["full_review_example.iter", "minimal.iter"],
    );
    expect(results).toEqual([
      { kind: "recent", path: "session_review.iter" },
      { kind: "file", path: "review.iter" },
      { kind: "example", name: "full_review_example.iter" },
    ]);
  });

  it("dedups: a path in both recents and files appears once as 'recent'", () => {
    const results = buildSearchResults(
      "plan",
      ["plan.iter"],
      [file("plan.iter"), file("planner.iter")],
      [],
    );
    expect(results).toEqual([
      { kind: "recent", path: "plan.iter" },
      { kind: "file", path: "planner.iter" },
    ]);
    // Sanity: "plan.iter" is not also reported as a "file" hit.
    expect(results.filter((r) => r.kind === "file" && r.path === "plan.iter")).toHaveLength(0);
  });

  it("matches case-insensitively", () => {
    const results = buildSearchResults(
      "REVIEW",
      ["Session_Review.iter"],
      [file("REVIEW.iter")],
      ["full_review.iter"],
    );
    expect(results.map((r) => (r.kind === "example" ? r.name : r.path))).toEqual([
      "Session_Review.iter",
      "REVIEW.iter",
      "full_review.iter",
    ]);
  });

  it("preserves source order: recents first, then files, then examples", () => {
    const results = buildSearchResults(
      "x",
      ["x_recent_2.iter", "x_recent_1.iter"], // recents are most-recent-first
      [file("x_file_a.iter"), file("x_file_b.iter")],
      ["x_example_a.iter", "x_example_b.iter"],
    );
    expect(results).toEqual([
      { kind: "recent", path: "x_recent_2.iter" },
      { kind: "recent", path: "x_recent_1.iter" },
      { kind: "file", path: "x_file_a.iter" },
      { kind: "file", path: "x_file_b.iter" },
      { kind: "example", name: "x_example_a.iter" },
      { kind: "example", name: "x_example_b.iter" },
    ]);
  });

  it("returns nothing when nothing matches", () => {
    const results = buildSearchResults(
      "zzz",
      ["plan.iter"],
      [file("review.iter")],
      ["minimal.iter"],
    );
    expect(results).toEqual([]);
  });

  it("supports paths with directory components", () => {
    const results = buildSearchResults(
      "subdir",
      ["subdir/a.iter"],
      [file("other/subdir/b.iter")],
      [],
    );
    expect(results).toEqual([
      { kind: "recent", path: "subdir/a.iter" },
      { kind: "file", path: "other/subdir/b.iter" },
    ]);
  });
});
