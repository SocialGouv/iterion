// @vitest-environment jsdom

import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { FirstClassBot } from "./firstClassBots";
import { useSessionLifecycle } from "./useSessionLifecycle";

const api = vi.hoisted(() => ({
  createRun: vi.fn(),
  getRunWithRetry: vi.fn(),
}));

const runState = vi.hoisted(() => ({
  applySnapshot: vi.fn(),
  reset: vi.fn(),
  loadEventHistoryIfMissing: vi.fn(),
  setRunId: vi.fn(),
}));

vi.mock("@/api/runs", () => api);
vi.mock("@/store/run", () => ({
  useRunStoreInstance: () => ({ getState: () => runState }),
  useRunStore: (selector: (state: typeof runState) => unknown) =>
    selector(runState),
}));

const bot: FirstClassBot = {
  id: "copilot",
  label: "Copi",
  description: "Assistant",
  workflowPath: "bots/copilot/main.bot",
  launcherVars: [],
  nodeMap: {},
};

describe("useSessionLifecycle", () => {
  beforeEach(() => vi.resetAllMocks());

  it("rejects a failed launch so the composer keeps its draft and references", async () => {
    const failure = new Error("launch unavailable");
    api.createRun.mockRejectedValueOnce(failure);
    const setStatus = vi.fn();
    const setErrorMessage = vi.fn();
    const { result } = renderHook(() =>
      useSessionLifecycle({
        bot,
        scopeKey: "project-a",
        repoScopeEnabled: false,
        activeRepo: null,
        lifetimeAbortRef: { current: new AbortController() },
        setRunId: vi.fn(),
        setStatus,
        setBusyMessageId: vi.fn(),
        setErrorMessage,
      }),
    );

    await act(async () => {
      await expect(result.current.launch({ initial_message: "help" })).rejects.toBe(
        failure,
      );
    });
    expect(setErrorMessage).toHaveBeenLastCalledWith("launch unavailable");
    expect(setStatus).toHaveBeenLastCalledWith("idle");
  });

  it("persists the dock tab before launching with typed Studio-chat provenance", async () => {
    const order: string[] = [];
    api.createRun.mockImplementationOnce(async () => {
      order.push("create");
      return { run_id: "run-1", status: "running" };
    });
    api.getRunWithRetry.mockRejectedValueOnce(new Error("snapshot not ready"));
    const beforeLaunch = vi.fn(() => order.push("persist"));
    const runSource = {
      kind: "studio_chat" as const,
      client_id: "client-1",
      conversation_id: "conversation-1",
    };
    const { result } = renderHook(() =>
      useSessionLifecycle({
        bot,
        scopeKey: "project-a",
        repoScopeEnabled: false,
        activeRepo: null,
        lifetimeAbortRef: { current: new AbortController() },
        setRunId: vi.fn(),
        setStatus: vi.fn(),
        setBusyMessageId: vi.fn(),
        setErrorMessage: vi.fn(),
        runSource,
        beforeLaunch,
      }),
    );

    await act(async () => {
      await result.current.launch({ initial_message: "help" });
    });

    expect(order).toEqual(["persist", "create"]);
    expect(api.createRun).toHaveBeenCalledWith(
      expect.objectContaining({ run_source: runSource }),
    );
  });
});
