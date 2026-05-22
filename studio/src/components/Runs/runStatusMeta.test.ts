import { describe, expect, it } from "vitest";

import {
  isActiveStatus,
  labelForStatus,
  STATUS_VARIANT,
} from "./runStatusMeta";

describe("labelForStatus", () => {
  it("humanises the paused/failed variants with an em-dash separator", () => {
    expect(labelForStatus("paused_waiting_human")).toBe("Paused — input needed");
    expect(labelForStatus("paused_operator")).toBe("Paused — operator");
    expect(labelForStatus("failed_resumable")).toBe("Failed — resumable");
  });

  it("keeps queued capitalised and falls through to the raw status for the rest", () => {
    expect(labelForStatus("queued")).toBe("Queued");
    expect(labelForStatus("running")).toBe("running");
    expect(labelForStatus("finished")).toBe("finished");
    expect(labelForStatus("failed")).toBe("failed");
    expect(labelForStatus("cancelled")).toBe("cancelled");
  });
});

describe("STATUS_VARIANT", () => {
  it("maps every RunStatus to a badge variant", () => {
    // Spot-check a few well-known mappings stay stable — the badge
    // variant drives chip colour across the studio.
    expect(STATUS_VARIANT.running).toBe("info");
    expect(STATUS_VARIANT.failed).toBe("danger");
    expect(STATUS_VARIANT.finished).toBe("success");
    expect(STATUS_VARIANT.paused_waiting_human).toBe("warning");
    expect(STATUS_VARIANT.paused_operator).toBe("info");
    expect(STATUS_VARIANT.queued).toBe("neutral");
  });
});

describe("isActiveStatus", () => {
  it("treats running and queued as active, everything else as inactive", () => {
    expect(isActiveStatus("running")).toBe(true);
    expect(isActiveStatus("queued")).toBe(true);
    expect(isActiveStatus("paused_waiting_human")).toBe(false);
    expect(isActiveStatus("paused_operator")).toBe(false);
    expect(isActiveStatus("finished")).toBe(false);
    expect(isActiveStatus("failed")).toBe(false);
    expect(isActiveStatus("failed_resumable")).toBe(false);
    expect(isActiveStatus("cancelled")).toBe(false);
  });
});
