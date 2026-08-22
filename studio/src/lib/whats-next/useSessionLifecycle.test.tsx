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
});
