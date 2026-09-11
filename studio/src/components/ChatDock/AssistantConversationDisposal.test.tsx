// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "@/api/client";
import {
  readConversations,
  writeActiveConversation,
  writeConversations,
} from "@/lib/chatDock/conversations";
import { useUIStore } from "@/store/ui";

import { AssistantProvider, useAssistantDock, useAssistantSession } from "./AssistantProvider";

const { cancelRunMock, bots } = vi.hoisted(() => ({
  cancelRunMock: vi.fn(),
  bots: [
    {
      id: "copilot",
      label: "Copi",
      description: "",
      workflowPath: "bots/copilot/main.bot",
      launcherVars: [],
      nodeMap: {},
    },
    {
      id: "other",
      label: "Other",
      description: "",
      workflowPath: "bots/other/main.bot",
      launcherVars: [],
      nodeMap: {},
    },
  ],
}));

vi.mock("@/api/runs", () => ({
  cancelRun: cancelRunMock,
  // Reconciliation is orthogonal to disposal in this suite. Returning an
  // empty inventory prevents its startup request from manufacturing a Retry
  // toast that races the close assertion.
  listRuns: vi.fn().mockResolvedValue([]),
}));

vi.mock("@/hooks/useChatRegistry", () => ({
  useChatRegistry: () => ({
    byId: Object.fromEntries(bots.map((bot) => [bot.id, bot])),
    bots,
    dockBots: bots,
    resolve: (id: string) => bots.find((bot) => bot.id === id) ?? bots[0],
    resolveDock: (id: string) => bots.find((bot) => bot.id === id) ?? bots[0],
    loading: false,
    error: null,
  }),
}));

vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  useWhatsNextSession: () => ({
    status: "idle",
    runId: null,
    messages: [],
    busyMessageId: null,
    runStatus: null,
    errorMessage: null,
    lastVars: null,
    discoveryError: null,
    retryDiscovery: () => {},
    sessionRepo: null,
    launchRepo: null,
    launch: async () => {},
    submitHumanAnswer: async () => {},
    newSession: () => {},
    resume: async () => {},
  }),
}));

function Probe() {
  const dock = useAssistantDock();
  const session = useAssistantSession();
  const active = dock?.activeConversationId ?? "none";
  return (
    <>
      <span data-testid="active">{active}</span>
      <span data-testid="bot">{session?.bot?.id ?? "none"}</span>
      <span data-testid="closing">
        {dock?.closingConversationIds.has(active) ? "yes" : "no"}
      </span>
      <button
        type="button"
        disabled={dock?.closingConversationIds.has(active)}
        onClick={() => void dock?.closeConversationById(active)}
      >
        close
      </button>
      <button type="button" onClick={() => void dock?.closeConversationById("c-2")}>
        close second
      </button>
      <button type="button" onClick={() => session?.selectBot("other")}>
        switch
      </button>
    </>
  );
}

function seed() {
  writeConversations([
    { id: "c-1", botId: "copilot", runId: "run-owned-without-snapshot" },
  ]);
  writeActiveConversation("c-1");
}

function deferredCancel() {
  let resolve!: (value: { run_id: string; status: string }) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<{ run_id: string; status: string }>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  cancelRunMock.mockReturnValue(promise);
  return { resolve, reject };
}

beforeEach(() => {
  localStorage.clear();
  useUIStore.setState({ toasts: [] });
  cancelRunMock.mockReset();
  window.history.pushState({}, "", "/board");
  seed();
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

describe("AssistantProvider conversation disposal", () => {
  it("uses the persisted run id and keeps the tab until HTTP acceptance", async () => {
    const pending = deferredCancel();
    render(<AssistantProvider><Probe /></AssistantProvider>);

    fireEvent.click(screen.getByRole("button", { name: "close" }));
    fireEvent.click(await screen.findByRole("button", { name: "Close and stop run" }));

    await waitFor(() => expect(cancelRunMock).toHaveBeenCalledOnce());
    expect(cancelRunMock).toHaveBeenCalledWith("run-owned-without-snapshot");
    expect(screen.getByTestId("active").textContent).toBe("c-1");
    expect(screen.getByTestId("closing").textContent).toBe("yes");
    expect((screen.getByRole("button", { name: "close" }) as HTMLButtonElement).disabled).toBe(true);

    await act(async () => {
      pending.resolve({ run_id: "run-owned-without-snapshot", status: "cancelling" });
      await Promise.resolve();
    });
    await waitFor(() => expect(readConversations()).toEqual([]));
    expect(screen.getByTestId("active").textContent).not.toBe("c-1");
  });

  it("deduplicates close gestures before React can disable the control", async () => {
    const pending = deferredCancel();
    render(<AssistantProvider><Probe /></AssistantProvider>);
    const close = screen.getByRole("button", { name: "close" });
    fireEvent.click(close);
    fireEvent.click(close);
    expect(screen.getAllByRole("button", { name: "Close and stop run" })).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Close and stop run" }));
    await waitFor(() => expect(cancelRunMock).toHaveBeenCalledOnce());
    await act(async () => {
      pending.resolve({ run_id: "run-owned-without-snapshot", status: "cancelling" });
      await Promise.resolve();
    });
  });

  it("serializes confirmations across different conversations", () => {
    writeConversations([
      { id: "c-1", botId: "copilot", runId: "run-1" },
      { id: "c-2", botId: "copilot", runId: "run-2" },
    ]);
    writeActiveConversation("c-1");
    render(<AssistantProvider><Probe /></AssistantProvider>);
    fireEvent.click(screen.getByRole("button", { name: "close" }));
    // Radix correctly aria-hides the app behind the modal; direct text lookup
    // still lets this test exercise the synchronous provider guard.
    fireEvent.click(screen.getByText("close second"));
    expect(screen.getAllByRole("button", { name: "Close and stop run" })).toHaveLength(1);
    expect(screen.getByTestId("closing").textContent).toBe("yes");
    expect(cancelRunMock).not.toHaveBeenCalled();
  });

  it.each([404, 410])("closes when cancellation reports HTTP %s", async (status) => {
    cancelRunMock.mockRejectedValue(new ApiError(status, "gone"));
    render(<AssistantProvider><Probe /></AssistantProvider>);
    fireEvent.click(screen.getByRole("button", { name: "close" }));
    fireEvent.click(await screen.findByRole("button", { name: "Close and stop run" }));
    await waitFor(() => expect(readConversations()).toEqual([]));
    expect(useUIStore.getState().toasts).toEqual([]);
  });

  it("keeps the tab and exposes Retry close on a server failure", async () => {
    cancelRunMock.mockRejectedValue(new ApiError(503, "unavailable"));
    render(<AssistantProvider><Probe /></AssistantProvider>);
    fireEvent.click(screen.getByRole("button", { name: "close" }));
    fireEvent.click(await screen.findByRole("button", { name: "Close and stop run" }));

    await waitFor(() => expect(useUIStore.getState().toasts).toHaveLength(1));
    expect(readConversations()[0]?.id).toBe("c-1");
    expect(useUIStore.getState().toasts[0]?.action?.label).toBe("Retry close");
    expect(screen.getByTestId("closing").textContent).toBe("no");
  });

  it("cancels before switching bots instead of orphaning the previous run", async () => {
    cancelRunMock.mockResolvedValue({ run_id: "run-owned-without-snapshot", status: "cancelling" });
    render(<AssistantProvider><Probe /></AssistantProvider>);
    fireEvent.click(screen.getByRole("button", { name: "switch" }));
    fireEvent.click(await screen.findByRole("button", { name: "Stop run and switch" }));

    await waitFor(() => expect(readConversations()[0]?.botId).toBe("other"));
    expect(cancelRunMock).toHaveBeenCalledWith("run-owned-without-snapshot");
    expect(readConversations()[0]?.runId).toBeUndefined();
    await waitFor(() => expect(screen.getByTestId("bot").textContent).toBe("other"));
  });
});
