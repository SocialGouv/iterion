// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const state = vi.hoisted(() => ({
  request: {
    key: "run:nexie:1:0",
    id: "board.issue.transition",
    intent: "explicit",
    args: { issue_id: "issue-1", to: "ready" },
  } as {
    key: string;
    id:
      | "board.issue.transition"
      | "pipeline.task.create"
      | "bot.create"
      | "run.launch"
      | "run.resume"
      | "run.watch"
      | "workspace.handoff.complete";
    intent: "explicit" | "suggested";
    args: Record<string, unknown>;
  },
  execute: vi.fn(),
}));

vi.mock("@/hooks/useAssistantActions", () => ({
  useAssistantActions: () => [state.request],
}));
vi.mock("@/lib/chatDock/assistantActionRequests", async (importOriginal) => {
  const actual = await importOriginal<
    typeof import("@/lib/chatDock/assistantActionRequests")
  >();
  return { ...actual, executeAssistantAction: state.execute };
});

import { writeAssistantActionPolicy } from "@/lib/chatDock/assistantActions";
import * as runsApi from "@/api/runs";
import AssistantActionOffer from "./AssistantActionOffer";

function renderOffer(workspaceHandoffId: string | null = null) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <AssistantActionOffer
        runId="assistant-run"
        revision={1}
        workspaceHandoffId={workspaceHandoffId}
      />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
	vi.resetAllMocks();
	vi.spyOn(runsApi, "deliverHostEvent").mockResolvedValue({ delivered: true });
  localStorage.clear();
  sessionStorage.clear();
  state.request = {
    key: "run:nexie:1:0",
    id: "board.issue.transition",
    intent: "explicit",
    args: { issue_id: "issue-1", to: "ready" },
  };
  state.execute.mockResolvedValue({ message: "Moved issue-1 to ready" });
});
afterEach(cleanup);

describe("AssistantActionOffer", () => {
  it("safely retries an interrupted terminal handoff receipt", async () => {
    state.request = {
      key: "run:copi:handoff-complete:0",
      id: "workspace.handoff.complete",
      intent: "suggested",
      args: { status: "completed", summary: "Verified the shared bot update" },
    };
    localStorage.setItem(
      "iterion.assistant.executedAction.v2:run:copi:handoff-complete:0",
      JSON.stringify({ status: "running" }),
    );
    state.execute.mockResolvedValue({
      message: "Reported this result to the originating Copi conversation",
    });

    renderOffer("handoff-1");

    await waitFor(() => expect(state.execute).toHaveBeenCalledTimes(1));
    expect(state.execute).toHaveBeenCalledWith(expect.anything(), {
      assistantRunId: "assistant-run",
      workspaceHandoffId: "handoff-1",
      forceResume: false,
    });
    expect(screen.queryByRole("button", { name: "Confirm action" })).toBeNull();
  });

  it("asks by default and executes only after confirmation", async () => {
    const view = renderOffer();
    expect(state.execute).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));
    await screen.findByText("Moved issue-1 to ready");
    expect(state.execute).toHaveBeenCalledTimes(1);

    // A persisted conversation is restored after a full browser/computer
    // restart, while sessionStorage is not. The receipt must still suppress
    // replay of the old mutation.
    sessionStorage.clear();
    view.unmount();
    renderOffer();
    expect(await screen.findByText("Moved issue-1 to ready")).toBeTruthy();
    expect(state.execute).toHaveBeenCalledTimes(1);
  });

  it("delivers only the host-bound created-bot editor receipt", async () => {
    state.request = {
      key: "run:copi:create-bot:0",
      id: "bot.create",
      intent: "explicit",
      args: {
        slug: "expense-approval",
        instructions: "Route expense approval requests.",
      },
    };
    state.execute.mockResolvedValue({
      message: "Created bot Expense approval",
      receipt: {
        created_bot: {
          name: "expense-approval",
          editor_path: "bots/expense-approval/main.bot",
        },
      },
    });

    renderOffer();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));

    await waitFor(() => {
      expect(runsApi.deliverHostEvent).toHaveBeenCalledWith(
        "assistant-run",
        "action-completed",
        expect.objectContaining({
          action: "bot.create",
          receipt: {
            created_bot: {
              name: "expense-approval",
              editor_path: "bots/expense-approval/main.bot",
            },
          },
        }),
      );
    });
  });

  it("enforces a denied policy", () => {
    writeAssistantActionPolicy("board.issue.transition", "deny");
    renderOffer();
    expect(screen.getByText(/blocked by settings/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Confirm action" })).toBeNull();
    expect(state.execute).not.toHaveBeenCalled();
  });

  it("turns a stale failed-run resume into a terminal explanation", async () => {
    state.request = {
      key: "run:copi:failed:0",
      id: "run.resume",
      intent: "explicit",
      args: { run_id: "run-1" },
    };
    state.execute.mockRejectedValueOnce(
      new Error(
        'API error 400: resume: run "run-1" cannot be resumed (status: failed)',
      ),
    );

    renderOffer();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));

    expect(
      await screen.findByText(/has since reached failed/i),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(
      screen.getByRole("link", { name: "Open run" }).getAttribute("href"),
    ).toBe("/runs/run-1");
  });

  it("migrates an impossible delegated launch to a terminal card", async () => {
    state.request = {
      key: "run:copi:delegated:0",
      id: "run.launch",
      intent: "suggested",
      args: {
        bot: "legendary-film-chapter",
        source_run_id: "failed-1",
        instructions: "Repair timing.",
      },
    };
    localStorage.setItem(
      "iterion.assistant.executedAction.v2:run:copi:delegated:0",
      JSON.stringify({
        status: "error",
        message:
          "API error 422: delegated launch: delegated worker must declare worktree: auto",
      }),
    );

    renderOffer();

    expect(
      await screen.findByText(/cannot repair a failed source run/i),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(
      screen
        .getByRole("link", { name: "Open failed run" })
        .getAttribute("href"),
    ).toBe("/runs/failed-1");
    expect(state.execute).not.toHaveBeenCalled();
  });

  it("renders an invalid string priority without offering or executing it", () => {
    state.request = {
      key: "run:copi:priority:0",
      id: "pipeline.task.create",
      intent: "explicit",
      args: {
        bot: "dev-squad",
        title: "Fix candidate authority",
        priority: "high",
        start: true,
      },
    };

    renderOffer();

    expect(screen.getByText("Invalid assistant action")).toBeTruthy();
    expect(screen.getByText(/received "high"/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Confirm action" })).toBeNull();
    expect(state.execute).not.toHaveBeenCalled();
  });

  it("auto-executes an explicitly requested action when configured", async () => {
    writeAssistantActionPolicy("board.issue.transition", "explicit");
    renderOffer();
    await waitFor(() => expect(state.execute).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Moved issue-1 to ready")).toBeTruthy();
  });

  it("forwards only an exact host-derived completion receipt to Copi", async () => {
    state.execute.mockResolvedValueOnce({
      message: "Created commit 0123456789ab",
      receipt: { git_commit: "0123456789abcdef0123456789abcdef01234567" },
    });
    renderOffer();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));

    await waitFor(() => {
      expect(runsApi.deliverHostEvent).toHaveBeenCalledWith(
        "assistant-run",
        "action-completed",
        expect.objectContaining({
          action: "board.issue.transition",
          receipt: { git_commit: "0123456789abcdef0123456789abcdef01234567" },
        }),
      );
    });
  });

  it("omits a malformed completion receipt from Copi's host event", async () => {
    state.execute.mockResolvedValueOnce({
      message: "Created commit 0123456789ab",
      receipt: { git_commit: "0123456789abcdef" },
    });
    renderOffer();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));

    await waitFor(() => {
      expect(runsApi.deliverHostEvent).toHaveBeenCalledWith(
        "assistant-run",
        "action-completed",
        expect.not.objectContaining({ receipt: expect.anything() }),
      );
    });
  });

  it("tells Copi when a confirmed action fails without exposing a token", async () => {
    state.request = {
      key: "run:copi:watch-failed:0",
      id: "run.watch",
      intent: "explicit",
      args: {
        target_run_id: "run-target",
        mode: "propose",
        kinds: ["run.failed", "run.stalled"],
      },
    };
    state.execute.mockRejectedValueOnce(
      new Error("API error 409: token=super-secret this run already has an active assistant watch"),
    );

    renderOffer();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));

    await waitFor(() => {
      expect(runsApi.deliverHostEvent).toHaveBeenCalledWith(
        "assistant-run",
        "action-failed",
        expect.objectContaining({
          action: "run.watch",
          args: expect.objectContaining({ target_run_id: "run-target" }),
          message: "API error 409: token=[redacted] this run already has an active assistant watch",
        }),
      );
      expect(runsApi.deliverHostEvent).toHaveBeenCalledWith(
        "assistant-run",
        "action-failed",
        expect.not.objectContaining({ receipt: expect.anything() }),
      );
    });
    expect(await screen.findByText(/already has an active assistant watch/i)).toBeTruthy();
  });

  it("requires a second explicit gesture before force-resuming source drift", async () => {
    state.request = {
      key: "run:copi:2:0",
      id: "run.resume",
      intent: "explicit",
      args: { run_id: "run-1" },
    };
    state.execute
      .mockRejectedValueOnce(
        new Error(
          'resume: runtime: workflow source has changed since run "run-1" was started',
        ),
      )
      .mockResolvedValueOnce({
        message: "Resumed run run-1 with the current workflow source",
      });

    renderOffer();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));

    const force = await screen.findByRole("button", {
      name: "Resume with updated workflow",
    });
    expect(state.execute).toHaveBeenNthCalledWith(1, expect.anything(), {
      assistantRunId: "assistant-run",
      forceResume: false,
    });
    expect(runsApi.deliverHostEvent).not.toHaveBeenCalled();
    expect(
      localStorage.getItem(
        "iterion.assistant.executedAction.v2:run:copi:2:0",
      ),
    ).toContain('"status":"source-drift"');

    fireEvent.click(force);
    expect(
      await screen.findByText(
        "Resumed run run-1 with the current workflow source",
      ),
    ).toBeTruthy();
    expect(state.execute).toHaveBeenNthCalledWith(2, expect.anything(), {
      assistantRunId: "assistant-run",
      forceResume: true,
    });
    await waitFor(() => {
      expect(runsApi.deliverHostEvent).toHaveBeenCalledTimes(1);
      expect(runsApi.deliverHostEvent).toHaveBeenCalledWith(
        "assistant-run",
        "action-completed",
        expect.objectContaining({
          action: "run.resume",
          message: "Resumed run run-1 with the current workflow source",
        }),
      );
    });
  });

  it("reports a source-drift failure from the forced resume to Copi", async () => {
    state.request = {
      key: "run:copi:forced-failure:0",
      id: "run.resume",
      intent: "explicit",
      args: { run_id: "run-1" },
    };
    const sourceDrift = new Error(
      'resume: runtime: workflow source has changed since run "run-1" was started',
    );
    state.execute
      .mockRejectedValueOnce(sourceDrift)
      .mockRejectedValueOnce(sourceDrift);

    renderOffer();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));

    const force = await screen.findByRole("button", {
      name: "Resume with updated workflow",
    });
    expect(runsApi.deliverHostEvent).not.toHaveBeenCalled();

    fireEvent.click(force);

    await waitFor(() => {
      expect(runsApi.deliverHostEvent).toHaveBeenCalledTimes(1);
      expect(runsApi.deliverHostEvent).toHaveBeenCalledWith(
        "assistant-run",
        "action-failed",
        expect.objectContaining({
          action: "run.resume",
          message: expect.stringContaining("workflow source has changed"),
        }),
      );
    });
    expect(state.execute).toHaveBeenNthCalledWith(2, expect.anything(), {
      assistantRunId: "assistant-run",
      forceResume: true,
    });
  });

  it("migrates a persisted pre-fix source-drift error to the force choice", async () => {
    state.request = {
      key: "run:copi:old:0",
      id: "run.resume",
      intent: "explicit",
      args: { run_id: "run-1" },
    };
    sessionStorage.setItem(
      "iterion.assistant.executedAction.v1:run:copi:old:0",
      JSON.stringify({
        status: "error",
        message:
          'API error 400: workflow source has changed since run "run-1" was started',
      }),
    );

    renderOffer();

    expect(
      await screen.findByRole("button", {
        name: "Resume with updated workflow",
      }),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(state.execute).not.toHaveBeenCalled();
    expect(
      localStorage.getItem(
        "iterion.assistant.executedAction.v2:run:copi:old:0",
      ),
    ).toContain("source-drift");
  });
});
