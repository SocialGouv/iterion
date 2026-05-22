import { describe, expect, it } from "vitest";

import { humanStatus } from "./WhatsNextView";

describe("WhatsNextView.humanStatus", () => {
  it("shows progress phases with sentence-case + ellipsis", () => {
    expect(humanStatus("launching", null)).toBe("Launching…");
    expect(humanStatus("submitting", "running")).toBe("Submitting…");
  });

  it("uses the labelForStatus humanisation when the session has ended", () => {
    expect(humanStatus("ended", "finished")).toMatch(/^Ended · /);
    expect(humanStatus("ended", "failed_resumable")).toBe(
      "Ended · Failed — resumable",
    );
    expect(humanStatus("ended", null)).toBe("Ended · unknown");
  });

  it("calls out the waiting-for-reply gate", () => {
    expect(humanStatus("active", "paused_waiting_human")).toBe(
      "Waiting for your reply",
    );
  });

  it("falls back to Active for the generic in-flight case", () => {
    expect(humanStatus("active", "running")).toBe("Active");
    // Defensive: when raw is null but the hook says we're active, still Active.
    expect(humanStatus("active", null)).toBe("Active");
  });
});
