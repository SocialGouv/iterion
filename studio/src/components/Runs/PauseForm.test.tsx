// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "@/api/client";
import { WORKFLOW_SOURCE_CHANGED_ERROR_CODE } from "@/api/runs";

import PauseForm from "./PauseForm";

const resumeRun = vi.fn();

vi.mock("@/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/runs")>();
  return {
    ...actual,
    resumeRun: (...args: unknown[]) => resumeRun(...args),
  };
});

vi.mock("@/store/document", () => ({
  useDocumentStore: (
    select: (state: { currentSource: string | null }) => unknown,
  ) => select({ currentSource: "editor workflow" }),
}));

afterEach(() => {
  cleanup();
  resumeRun.mockReset();
});

function sourceChangedError() {
  // Deliberately omit the historical prose: rendering the retry proves the
  // component consumes errorCode rather than parsing this message.
  return new ApiError(
    400,
    "API error 400: resume rejected",
    WORKFLOW_SOURCE_CHANGED_ERROR_CODE,
  );
}

describe("PauseForm force resume", () => {
  it("submits the form values as they exist when force retry is clicked", async () => {
    resumeRun
      .mockRejectedValueOnce(sourceChangedError())
      .mockResolvedValueOnce({ run_id: "run-1", status: "running" });

    render(
      <PauseForm
        runId="run-1"
        questions={{ reviewer: "Reviewer", notes: "Notes" }}
        sourceOverride={null}
      />,
    );

    const textboxes = screen.getAllByRole("textbox");
    expect(textboxes).toHaveLength(2);
    const reviewer = textboxes[0];
    const notes = textboxes[1];
    if (!reviewer || !notes) throw new Error("expected reviewer and notes fields");
    fireEvent.change(reviewer, { target: { value: "old reviewer" } });
    fireEvent.change(notes, { target: { value: "initial notes" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit & Resume" }));

    const force = await screen.findByRole("button", {
      name: "Resume with updated workflow (force)",
    });
    expect(resumeRun).toHaveBeenNthCalledWith(1, "run-1", {
      answers: { reviewer: "old reviewer", notes: "initial notes" },
      source: undefined,
    });

    fireEvent.change(reviewer, { target: { value: "current reviewer" } });
    fireEvent.change(notes, { target: { value: "edited after rejection" } });
    fireEvent.click(force);

    await waitFor(() => expect(resumeRun).toHaveBeenCalledTimes(2));
    expect(resumeRun).toHaveBeenNthCalledWith(2, "run-1", {
      answers: {
        reviewer: "current reviewer",
        notes: "edited after rejection",
      },
      source: undefined,
      force: true,
    });
  });

  it("replays the original one-click permission decision", async () => {
    resumeRun
      .mockRejectedValueOnce(sourceChangedError())
      .mockResolvedValueOnce({ run_id: "run-1", status: "running" });

    render(
      <PauseForm
        runId="run-1"
        questions={{
          _permission: { tool: "Bash", input: { command: "pwd" } },
          ask_user_response: "Allow this command?",
        }}
        sourceOverride={null}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Allow once" }));
    const force = await screen.findByRole("button", {
      name: "Resume with updated workflow (force)",
    });
    fireEvent.click(force);

    await waitFor(() => expect(resumeRun).toHaveBeenCalledTimes(2));
    expect(resumeRun).toHaveBeenNthCalledWith(2, "run-1", {
      answers: { ask_user_response: "allow" },
      source: undefined,
      force: true,
    });
  });

  it("does not carry a rejected decision into another run", async () => {
    resumeRun
      .mockRejectedValueOnce(sourceChangedError())
      .mockResolvedValueOnce({ run_id: "run-2", status: "running" });

    const questions = {
      _permission: { tool: "Bash", input: { command: "pwd" } },
      ask_user_response: "Allow this command?",
    };
    const { rerender } = render(
      <PauseForm
        runId="run-1"
        questions={questions}
        sourceOverride={null}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Allow once" }));
    await screen.findByRole("button", {
      name: "Resume with updated workflow (force)",
    });

    rerender(
      <PauseForm
        runId="run-2"
        questions={questions}
        sourceOverride={null}
      />,
    );

    await waitFor(() =>
      expect(
        screen.queryByRole("button", {
          name: "Resume with updated workflow (force)",
        }),
      ).toBeNull(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Allow once" }));

    await waitFor(() => expect(resumeRun).toHaveBeenCalledTimes(2));
    expect(resumeRun).toHaveBeenNthCalledWith(2, "run-2", {
      answers: { ask_user_response: "allow" },
      source: undefined,
    });
  });
});
