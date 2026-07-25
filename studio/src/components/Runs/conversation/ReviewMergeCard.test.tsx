// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { HumanQuestionMessage } from "@/lib/runChat/types";

const apiMocks = vi.hoisted(() => ({
  getRun: vi.fn().mockResolvedValue({}),
  resumeRun: vi.fn().mockResolvedValue({ run_id: "run-guided", status: "running" }),
}));

vi.mock("@/api/runs", () => ({
  getRun: (...args: unknown[]) => apiMocks.getRun(...args),
  resumeRun: (...args: unknown[]) => apiMocks.resumeRun(...args),
}));

vi.mock("@/store/document", () => ({
  useDocumentStore: (select: (state: { currentSource: string | null }) => unknown) =>
    select({ currentSource: "editor source must not be sent by the board" }),
}));

vi.mock("@/store/run", () => ({
  useRunStore: (select: (state: Record<string, unknown>) => unknown) =>
    select({
      setRunStatus: vi.fn(),
      requestWsReconnect: vi.fn(),
      applySnapshot: vi.fn(),
      resyncEventsAfterResume: vi.fn(),
    }),
}));

import ReviewMergeCard from "./ReviewMergeCard";

function guidedMessage(): HumanQuestionMessage {
  return {
    kind: "human-question",
    id: "guided-turn-1",
    nodeId: "review_gate",
    prompt: "Test the change.",
    status: "pending",
    review: {
      turns: [{ role: "companion", content: "Play the candidate clip." }],
      posture: "human_required",
      mergeStrategy: "squash",
      mergeInto: "main",
      maxTurns: 4,
    },
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ReviewMergeCard — pipeline board handoff", () => {
  it("submits a dialogue reply with reserved review keys and lets the board refetch", async () => {
    const onResumed = vi.fn();
    render(
      <ReviewMergeCard
        runId="run-guided"
        message={guidedMessage()}
        sourceOverride={null}
        onResumed={onResumed}
      />,
    );

    expect(screen.getByText("Play the candidate clip.")).toBeTruthy();
    fireEvent.change(screen.getByPlaceholderText(/Reply to the reviewer/i), {
      target: { value: "Playback is smooth." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Send reply" }));

    await waitFor(() =>
      expect(apiMocks.resumeRun).toHaveBeenCalledWith("run-guided", {
        answers: {
          __review_action: "reply",
          __review_reply: "Playback is smooth.",
        },
        source: undefined,
      }),
    );
    expect(onResumed).toHaveBeenCalledTimes(1);
    expect(apiMocks.getRun).not.toHaveBeenCalled();
  });

  it("submits approve/merge and request-changes actions from the board", async () => {
    const firstResolved = vi.fn();
    const first = render(
      <ReviewMergeCard
        runId="run-guided"
        message={guidedMessage()}
        sourceOverride={null}
        onResumed={firstResolved}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Approve & merge" }));
    await waitFor(() =>
      expect(apiMocks.resumeRun).toHaveBeenLastCalledWith("run-guided", {
        answers: {
          __review_action: "approve_merge",
          __review_merge_strategy: "squash",
        },
        source: undefined,
      }),
    );
    expect(firstResolved).toHaveBeenCalledTimes(1);

    first.unmount();
    apiMocks.resumeRun.mockClear();
    const secondResolved = vi.fn();
    render(
      <ReviewMergeCard
        runId="run-guided"
        message={{ ...guidedMessage(), id: "guided-turn-2" }}
        sourceOverride={null}
        onResumed={secondResolved}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));
    await waitFor(() =>
      expect(apiMocks.resumeRun).toHaveBeenLastCalledWith("run-guided", {
        answers: { __review_action: "request_changes" },
        source: undefined,
      }),
    );
    expect(secondResolved).toHaveBeenCalledTimes(1);
  });
});
