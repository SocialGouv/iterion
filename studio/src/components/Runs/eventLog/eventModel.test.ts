import { describe, expect, it } from "vitest";

import { EVENT_BADGE, previewData } from "./eventModel";

describe("previewData", () => {
  it("distinguishes a proactive cooldown skip from a fresh fallback", () => {
    const preview = previewData({
      reason: "usage_window",
      cooldown: true,
      cooldown_until: "2026-08-24T12:20:00Z",
      attempts: 0,
    });

    expect(preview).toContain("cooldown=true");
    expect(preview).toContain("cooldown_until=2026-08-24T12:20:00Z");
    expect(preview).toContain("attempts=0");
  });
});

it("shows a schema mismatch as a warning with field names", () => {
  expect(EVENT_BADGE.node_output_schema_mismatch).toBe("bg-warning-soft text-warning-fg");
  const preview = previewData({ schema: "verdict", fields: ["ok", "reason"] });
  expect(preview).toContain("schema=verdict");
  expect(preview).toContain("fields=ok,reason");
});
