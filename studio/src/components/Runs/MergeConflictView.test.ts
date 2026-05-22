import { describe, expect, it } from "vitest";

import { __testables } from "./MergeConflictView";
import type { MergeConflictHunk } from "@/api/runs";

const { applyHunk, hasConflictMarkers, replacementLines } = __testables;

// Synthetic conflict content matching what `git merge --squash`
// produces for a one-hunk conflict. Line numbers below are 1-indexed
// per the wire format.
const content = [
  "alpha",
  "<<<<<<< HEAD",
  "ours-a",
  "ours-b",
  "=======",
  "theirs-a",
  ">>>>>>> feature",
  "zeta",
].join("\n");

const hunk: MergeConflictHunk = {
  start_line: 2,
  end_line: 7,
  ours_label: "HEAD",
  theirs_label: "feature",
  ours_lines: ["ours-a", "ours-b"],
  theirs_lines: ["theirs-a"],
};

describe("applyHunk", () => {
  it("Take ours replaces the marker region with the ours side", () => {
    const next = applyHunk(content, hunk, "ours");
    expect(next).toBe(["alpha", "ours-a", "ours-b", "zeta"].join("\n"));
  });

  it("Take theirs replaces with the incoming side", () => {
    const next = applyHunk(content, hunk, "theirs");
    expect(next).toBe(["alpha", "theirs-a", "zeta"].join("\n"));
  });

  it("Take both concatenates ours then theirs", () => {
    const next = applyHunk(content, hunk, "both");
    expect(next).toBe(
      ["alpha", "ours-a", "ours-b", "theirs-a", "zeta"].join("\n"),
    );
  });
});

describe("hasConflictMarkers", () => {
  it("detects unresolved files", () => {
    expect(hasConflictMarkers(content)).toBe(true);
  });

  it("returns false on resolved content", () => {
    expect(hasConflictMarkers("alpha\nbeta\n")).toBe(false);
  });

  it("detects diff3 base marker", () => {
    expect(hasConflictMarkers("a\n||||||| base\n")).toBe(true);
  });
});

describe("replacementLines", () => {
  it("returns ours / theirs / both arrays without mutation", () => {
    const ours = replacementLines(hunk, "ours");
    ours.push("mutated");
    expect(hunk.ours_lines).toEqual(["ours-a", "ours-b"]);
  });
});
