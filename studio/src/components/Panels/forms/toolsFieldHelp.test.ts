import { describe, expect, it } from "vitest";

import { toolsFieldHelp } from "./toolsFieldHelp";

describe("toolsFieldHelp", () => {
  // The hazard the caption exists for: an operator who removes the last chip
  // has DECLARED an empty list — the node now runs with no tools — and the
  // widget looks exactly as it does for a node that declared nothing, which
  // runs with the backend's whole toolset. The two captions must differ, and
  // each must name its own state.
  it("tells the declared-empty state apart from the undeclared one", () => {
    const declared = toolsFieldHelp([]);
    const unset = toolsFieldHelp(undefined);
    const named = toolsFieldHelp(["read_file"]);
    expect(declared).not.toBe(unset);
    expect(declared).toMatch(/NO tools/);
    expect(unset).toMatch(/backend's own toolset/);
    expect(named).toMatch(/Removing the last one/);
  });
});
