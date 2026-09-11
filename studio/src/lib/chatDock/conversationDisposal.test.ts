import { describe, expect, it, vi } from "vitest";

import { ApiError } from "@/api/client";

import {
  cancelThenDispose,
  isRunAlreadyGoneError,
  runIdForDisposal,
  shouldConfirmRunDisposal,
} from "./conversationDisposal";

describe("assistant conversation disposal", () => {
  it("prefers the conversation-owned run id over a stale snapshot", () => {
    expect(
      runIdForDisposal(
        { id: "c-1", botId: "copilot", runId: "owned" },
        { run: { id: "stale", status: "finished" } },
      ),
    ).toBe("owned");
  });

  it("falls back to the snapshot for a legacy conversation", () => {
    expect(
      runIdForDisposal(
        { id: "c-1", botId: "copilot" },
        { run: { id: "snapshot", status: "running" } },
      ),
    ).toBe("snapshot");
  });

  it("confirms unknown, live, paused, and resumable runs", () => {
    expect(shouldConfirmRunDisposal("run", null)).toBe(true);
    expect(shouldConfirmRunDisposal("run", "running")).toBe(true);
    expect(shouldConfirmRunDisposal("run", "paused_waiting_human")).toBe(true);
    expect(shouldConfirmRunDisposal("run", "failed_resumable")).toBe(true);
    expect(shouldConfirmRunDisposal("run", "finished")).toBe(false);
    expect(shouldConfirmRunDisposal(null, "running")).toBe(false);
  });

  it.each([404, 410])("treats HTTP %s as already disposed", async (status) => {
    const dispose = vi.fn();
    const cancel = vi.fn().mockRejectedValue(new ApiError(status, "gone"));
    await cancelThenDispose({ runId: "run", dispose, cancel });
    expect(cancel).toHaveBeenCalledWith("run");
    expect(dispose).toHaveBeenCalledOnce();
  });

  it("waits for cancellation acceptance before disposing", async () => {
    let accept!: () => void;
    const cancel = vi.fn(
      () => new Promise<{ run_id: string; status: string }>((resolve) => {
        accept = () => resolve({ run_id: "run", status: "cancelling" });
      }),
    );
    const dispose = vi.fn();
    const pending = cancelThenDispose({ runId: "run", dispose, cancel });
    expect(dispose).not.toHaveBeenCalled();
    accept();
    await pending;
    expect(dispose).toHaveBeenCalledOnce();
  });

  it("keeps the owner when cancellation fails", async () => {
    const dispose = vi.fn();
    await expect(
      cancelThenDispose({
        runId: "run",
        dispose,
        cancel: vi.fn().mockRejectedValue(new ApiError(503, "down")),
      }),
    ).rejects.toMatchObject({ status: 503 });
    expect(dispose).not.toHaveBeenCalled();
  });

  it("recognises typed and legacy gone errors narrowly", () => {
    expect(isRunAlreadyGoneError(new ApiError(404, "gone"))).toBe(true);
    expect(isRunAlreadyGoneError(new Error("API error 410: pruned"))).toBe(true);
    expect(isRunAlreadyGoneError(new ApiError(403, "forbidden"))).toBe(false);
  });
});
