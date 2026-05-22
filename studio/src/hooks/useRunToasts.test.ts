import { describe, expect, it } from "vitest";

import type { RunEvent } from "@/api/runs";

import { toastForEvent } from "./useRunToasts";

function mkEvent(partial: Partial<RunEvent>): RunEvent {
  return {
    seq: partial.seq ?? 1,
    timestamp: partial.timestamp ?? "2026-05-22T12:00:00Z",
    type: partial.type ?? "run_started",
    run_id: partial.run_id ?? "run_xx",
    data: partial.data ?? {},
    ...(partial.node_id !== undefined ? { node_id: partial.node_id } : {}),
    ...(partial.log_offset !== undefined ? { log_offset: partial.log_offset } : {}),
  };
}

describe("toastForEvent", () => {
  it("returns a success toast for run_finished", () => {
    expect(toastForEvent(mkEvent({ type: "run_finished" }))).toEqual({
      message: "Run finished",
      type: "success",
    });
  });

  it("surfaces the error message on run_failed", () => {
    expect(
      toastForEvent(
        mkEvent({ type: "run_failed", data: { error: "boom" } }),
      ),
    ).toEqual({ message: "Run failed: boom", type: "error" });
  });

  it("falls back to 'see logs' when the failure error string is missing", () => {
    expect(toastForEvent(mkEvent({ type: "run_failed" }))).toEqual({
      message: "Run failed: see logs",
      type: "error",
    });
  });

  it("warns on input-requested pauses but stays neutral on operator pauses", () => {
    expect(toastForEvent(mkEvent({ type: "run_paused" }))).toEqual({
      message: "Run paused — input requested",
      type: "warning",
    });
    expect(
      toastForEvent(
        mkEvent({ type: "run_paused", data: { reason: "operator" } }),
      ),
    ).toEqual({ message: "Run paused by operator", type: "info" });
  });

  it("uses the new 'exhausted' wording for budget_exceeded", () => {
    expect(
      toastForEvent(
        mkEvent({ type: "budget_exceeded", data: { dimension: "cost_usd" } }),
      ),
    ).toEqual({
      message: "Budget exhausted: cost_usd hit hard cap.",
      type: "error",
    });
  });

  it("returns null for events we don't toast (no spurious noise)", () => {
    expect(toastForEvent(mkEvent({ type: "node_started" }))).toBeNull();
    expect(toastForEvent(mkEvent({ type: "tool_called" }))).toBeNull();
    expect(toastForEvent(mkEvent({ type: "llm_request" }))).toBeNull();
  });
});
