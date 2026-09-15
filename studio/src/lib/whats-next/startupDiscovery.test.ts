import { beforeEach, describe, it, expect, vi } from "vitest";

import type { RunSummary } from "@/api/runs";
import type { RunStore } from "@/store/run";

import {
  attachSessionRun,
  candidateWorkflows,
  pickLiveRunId,
  runBelongsToBot,
} from "./startupDiscovery";

const api = vi.hoisted(() => ({
  getRunWithRetry: vi.fn(),
  listRuns: vi.fn(),
}));

vi.mock("@/api/runs", () => api);

beforeEach(() => vi.resetAllMocks());

function run(id: string, status: RunSummary["status"]): RunSummary {
  return { id, workflow_name: "whats_next", status } as RunSummary;
}

describe("candidateWorkflows", () => {
  it("probes the underscore spelling first, then the raw bot id", () => {
    expect(candidateWorkflows("whats-next")).toEqual([
      "whats_next",
      "whats-next",
    ]);
  });

  it("dedupes when the bot id has no hyphens", () => {
    expect(candidateWorkflows("nexie")).toEqual(["nexie"]);
  });

  it("replaces every hyphen, not just the first", () => {
    expect(candidateWorkflows("a-b-c")).toEqual(["a_b_c", "a-b-c"]);
  });
});

describe("pickLiveRunId", () => {
  it("returns null on an empty match list", () => {
    expect(pickLiveRunId([])).toBeNull();
  });

  it("picks the first non-terminal run in list order", () => {
    expect(
      pickLiveRunId([
        run("r1", "finished"),
        run("r2", "running"),
        run("r3", "paused_waiting_human"),
      ]),
    ).toBe("r2");
  });

  it("treats queued and paused_waiting_human as live", () => {
    expect(pickLiveRunId([run("q", "queued")])).toBe("q");
    expect(pickLiveRunId([run("p", "paused_waiting_human")])).toBe("p");
  });

  it("ignores terminal and operator-paused runs", () => {
    expect(
      pickLiveRunId([
        run("a", "finished"),
        run("b", "failed"),
        run("c", "failed_resumable"),
        run("d", "cancelled"),
        run("e", "paused_operator"),
      ]),
    ).toBeNull();
  });
});

describe("attachSessionRun", () => {
  it("returns false when identity changes during history hydration", async () => {
    let finishHistory!: () => void;
    const history = new Promise<void>((resolve) => {
      finishHistory = resolve;
    });
    const state = {
      reset: vi.fn(),
      applySnapshot: vi.fn(),
      setRunId: vi.fn(),
      loadEventHistoryIfMissing: vi.fn(() => history),
    };
    const store = { getState: () => state } as unknown as RunStore;
    api.getRunWithRetry.mockResolvedValueOnce({
      run: { id: "run-a", workflow_name: "whats_next" },
    });
    let cancelled = false;

    const attaching = attachSessionRun({
      store,
      runId: "run-a",
      botId: "whats-next",
      scopeKey: "project-a",
      signal: new AbortController().signal,
      isCancelled: () => cancelled,
    });
    await vi.waitFor(() =>
      expect(state.loadEventHistoryIfMissing).toHaveBeenCalledWith("run-a"),
    );
    cancelled = true;
    finishHistory();

    await expect(attaching).resolves.toBe(false);
  });

  it("refuses a run whose workflow is not this bot's", async () => {
    const state = {
      reset: vi.fn(),
      applySnapshot: vi.fn(),
      setRunId: vi.fn(),
      loadEventHistoryIfMissing: vi.fn(),
    };
    const store = { getState: () => state } as unknown as RunStore;
    api.getRunWithRetry.mockResolvedValueOnce({
      run: { id: "run-a", workflow_name: "copilot" },
    });

    await expect(
      attachSessionRun({
        store,
        runId: "run-a",
        botId: "whats-next",
        scopeKey: "project-a",
        signal: new AbortController().signal,
        isCancelled: () => false,
      }),
    ).resolves.toBe(false);
    expect(state.reset).not.toHaveBeenCalled();
    expect(state.applySnapshot).not.toHaveBeenCalled();
  });
});

describe("runBelongsToBot", () => {
  it("accepts hyphen and underscore spellings of the bot id", () => {
    expect(runBelongsToBot("whats_next", "whats-next")).toBe(true);
    expect(runBelongsToBot("whats-next", "whats-next")).toBe(true);
  });

  it("rejects another bot's workflow", () => {
    expect(runBelongsToBot("copilot", "whats-next")).toBe(false);
    expect(runBelongsToBot(undefined, "whats-next")).toBe(false);
  });
});
