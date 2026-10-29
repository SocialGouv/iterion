import { describe, expect, it } from "vitest";

import type { WebhookDelivery } from "@/api/webhooks";
import { runOutcomeFailed } from "@/components/shared/LastRunOutcome";

import { deliveryRunOutcome } from "./deliveryRunOutcome";

const launched: WebhookDelivery = {
  id: "d-1",
  tenant_id: "t1",
  webhook_id: "w1",
  provider: "github",
  idempotency_key: "key",
  status: "launched",
  run_id: "run-42",
  received_at: "2026-10-29T09:00:00Z",
};

describe("deliveryRunOutcome", () => {
  // The delivery is the fourth record kind of #1501: it must reach the SAME
  // component as the schedule row, the automation row and the board card, so
  // one tuple cannot read "failed" on one surface and nothing on another.
  it("maps run_id onto the shared record's id half", () => {
    expect(deliveryRunOutcome(launched).last_run_id).toBe("run-42");
  });

  it("carries the terminal status, code and instant the spine stamped", () => {
    const record = deliveryRunOutcome({
      ...launched,
      last_run_status: "failed",
      last_run_error: "sandbox: no runtime available",
      last_run_error_code: "SANDBOX_DRIVER_UNAVAILABLE",
      last_run_at: "2026-10-29T09:05:00Z",
    });
    expect(record).toEqual({
      last_run_id: "run-42",
      last_run_status: "failed",
      last_run_error: "sandbox: no runtime available",
      last_run_error_code: "SANDBOX_DRIVER_UNAVAILABLE",
      last_run_at: "2026-10-29T09:05:00Z",
    });
    expect(runOutcomeFailed(record)).toBe(true);
  });

  // A cancelled run carries no message at all: judged on the message it
  // would read as a success, which is the regression the shared helper's
  // status-first reading exists for — pinned here too, because the delivery
  // row is where a cancelled webhook run is most often read.
  it("reports a cancelled run as a failure though it carries no text", () => {
    const record = deliveryRunOutcome({ ...launched, last_run_status: "cancelled" });
    expect(record.last_run_error).toBeUndefined();
    expect(runOutcomeFailed(record)).toBe(true);
  });

  it("reports a finished run as a success", () => {
    expect(runOutcomeFailed(deliveryRunOutcome({ ...launched, last_run_status: "finished" }))).toBe(
      false,
    );
  });

  // The delivery's own refusal has its own column in the drawer. Mapping it
  // onto last_error too would render one failure as two.
  it("does not re-render the delivery's own verdict as a launch refusal", () => {
    const record = deliveryRunOutcome({
      ...launched,
      status: "launch_error",
      error: "cloudpublisher: publish: nats: no responders",
    });
    expect(record.last_error).toBeUndefined();
  });
});
