// @vitest-environment jsdom
import { cleanup, render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

import { LastRunSection } from "./LastRunSection";

// Mock the runs API: getRun feeds the paused-run detection, resumeRun is
// what the answer affordance must call.
const getRun = vi.fn();
const resumeRun = vi.fn().mockResolvedValue({ run_id: "r1", status: "running" });
vi.mock("@/api/runs", () => ({
  getRun: (...a: unknown[]) => getRun(...a),
  resumeRun: (...a: unknown[]) => resumeRun(...a),
}));

// The document store backs PauseForm's editor-buffer source; the board
// caller overrides it, so its value must NOT reach resumeRun.
vi.mock("@/store/document", () => ({
  useDocumentStore: (sel: (s: { currentSource: string | null }) => unknown) =>
    sel({ currentSource: "some-other-bot.bot" }),
}));

// BranchDiffModal pulls a heavy dependency chain we don't exercise here.
vi.mock("@/components/Runs/BranchDiffModal", () => ({ default: () => null }));

function renderSection(runID: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LastRunSection runID={runID} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("LastRunSection answer-from-board affordance", () => {
  it("renders the pause affordance and resumes with NO source (falls back to persisted FilePath)", async () => {
    getRun.mockResolvedValue({
      run: {
        id: "r1",
        status: "paused_waiting_human",
        checkpoint: {
          node_id: "ask_reviewer",
          interaction_id: "r1_ask_reviewer",
          interaction_questions: { decision: "Ship it?" },
        },
      },
      executions: [],
      last_seq: 0,
    });

    renderSection("r1");

    await waitFor(() => expect(screen.getByText(/Awaiting input/i)).toBeTruthy());
    // The question field label from PauseForm is rendered.
    await waitFor(() => expect(screen.getByText(/decision/i)).toBeTruthy());

    const textarea = document.querySelector("textarea");
    expect(textarea).toBeTruthy();
    fireEvent.change(textarea as HTMLTextAreaElement, { target: { value: "yes" } });
    const submit = screen.getByRole("button", { name: /submit|send|resume/i });
    fireEvent.click(submit);

    await waitFor(() => expect(resumeRun).toHaveBeenCalledTimes(1));
    const [runId, body] = resumeRun.mock.calls[0];
    expect(runId).toBe("r1");
    expect(body.answers).toEqual({ decision: "yes" });
    // Critical: the editor buffer must NOT leak in as the resume source.
    expect(body.source).toBeUndefined();
  });

  it("shows only the plain last-run panel when the run is not paused", async () => {
    getRun.mockResolvedValue({
      run: { id: "r1", status: "finished", checkpoint: {} },
      executions: [],
      last_seq: 0,
    });
    renderSection("r1");
    await waitFor(() => expect(screen.getByText(/Last run/i)).toBeTruthy());
    expect(screen.queryByText(/Awaiting input/i)).toBeNull();
  });
});
