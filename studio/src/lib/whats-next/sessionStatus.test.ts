import { describe, it, expect } from "vitest";

import { isEndedRunStatus } from "./sessionStatus";

describe("isEndedRunStatus", () => {
  it("classifies the four ended statuses (incl. failed_resumable)", () => {
    expect(isEndedRunStatus("finished")).toBe(true);
    expect(isEndedRunStatus("failed")).toBe(true);
    expect(isEndedRunStatus("cancelled")).toBe(true);
    expect(isEndedRunStatus("failed_resumable")).toBe(true);
  });

  it("keeps live and paused statuses active", () => {
    expect(isEndedRunStatus("queued")).toBe(false);
    expect(isEndedRunStatus("running")).toBe(false);
    expect(isEndedRunStatus("paused_waiting_human")).toBe(false);
    expect(isEndedRunStatus("paused_operator")).toBe(false);
  });
});
