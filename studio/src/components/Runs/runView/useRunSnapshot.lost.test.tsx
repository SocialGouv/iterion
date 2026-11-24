// @vitest-environment jsdom
// The status safety-net poll stops once the run socket closed scope_lost:
// the ID may name the original's successor now, so the view has nothing to
// fetch by it. (The store drops such a snapshot as well; this witness pins
// the poll's own guard by counting the requests it makes.)
import { act, cleanup, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { RunSnapshot } from "@/api/runs";

const successor = {
  run: { id: "run-a", status: "running", workflow_name: "successor-wf", name: "successor", created_at: "", updated_at: "" },
  executions: [],
  last_seq: 99,
} as unknown as RunSnapshot;
const getRun = vi.fn(async () => successor);
vi.mock("@/api/runs", async (orig) => {
  const real = (await orig()) as Record<string, unknown>;
  return {
    ...real,
    getRun: () => getRun(),
    // The initial fetch is out of this witness: it never resolves a snapshot.
    getRunWithRetry: async () => {
      throw Object.assign(new Error("aborted"), { name: "AbortError" });
    },
  };
});

import { createRunStore, RunStoreProvider } from "@/store/run";

import { useRunSnapshot } from "./useRunSnapshot";

beforeEach(() => {
  vi.useFakeTimers();
  getRun.mockClear();
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function runningOrigin(wsState: "lost" | "closed") {
  const store = createRunStore();
  // History is out of this witness too: mark it fetched.
  store.setState({ historyFetchedForRun: "run-a" });
  store.getState().applySnapshot({
    run: { id: "run-a", status: "running", workflow_name: "origin-wf", name: "origin", created_at: "", updated_at: "" },
    executions: [],
    last_seq: 10,
  } as unknown as RunSnapshot);
  store.getState().setWsState(wsState);
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(RunStoreProvider, { store, children });
  renderHook(() => useRunSnapshot("run-a"), { wrapper });
  return store;
}

describe("the status poll of a running run", () => {
  it("fetches nothing once the socket is lost", async () => {
    const store = runningOrigin("lost");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(40_000);
    });
    expect(getRun).not.toHaveBeenCalled();
    expect(store.getState().snapshot?.run.name).toBe("origin");
  });

  it("polls a down socket that is not lost", async () => {
    runningOrigin("closed");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(12_500);
    });
    expect(getRun).toHaveBeenCalled();
  });
});
