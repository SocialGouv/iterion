// @vitest-environment jsdom
import { describe, expect, it } from "vitest";

import { statusFilterLabel } from "./RunListView";

// RunListView pulls in wouter and React-Query primitives at module
// scope; jsdom is the safe choice for the import even though the
// helper itself never touches the DOM.
describe("statusFilterLabel", () => {
  it("returns 'matching' as the fragment when no status filter is active", () => {
    expect(statusFilterLabel("")).toBe("matching");
  });

  it("uses the chip's lower-cased label for canonical statuses", () => {
    expect(statusFilterLabel("running")).toBe("running");
    expect(statusFilterLabel("finished")).toBe("finished");
    expect(statusFilterLabel("queued")).toBe("queued");
    expect(statusFilterLabel("paused_waiting_human")).toBe("paused");
  });

  it("falls back to the labelForStatus humanisation for resumable failures", () => {
    // RunListView's chip uses "Failed (resumable)" — we expect the
    // lower-cased chip text, not the labelForStatus em-dash form
    // (the chip is authoritative because that's what the user just
    // clicked).
    expect(statusFilterLabel("failed_resumable")).toBe("failed (resumable)");
  });
});
