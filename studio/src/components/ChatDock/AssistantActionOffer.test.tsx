// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const state = vi.hoisted(() => ({
  request: {
    key: "run:nexie:1:0",
    id: "board.issue.transition" as const,
    intent: "explicit" as const,
    args: { issue_id: "issue-1", to: "ready" },
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
import AssistantActionOffer from "./AssistantActionOffer";

function renderOffer() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <AssistantActionOffer runId="assistant-run" revision={1} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  localStorage.clear();
  sessionStorage.clear();
  state.execute.mockResolvedValue({ message: "Moved issue-1 to ready" });
});
afterEach(cleanup);

describe("AssistantActionOffer", () => {
  it("asks by default and executes only after confirmation", async () => {
    const view = renderOffer();
    expect(state.execute).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Confirm action" }));
    await screen.findByText("Moved issue-1 to ready");
    expect(state.execute).toHaveBeenCalledTimes(1);

    view.unmount();
    renderOffer();
    expect(await screen.findByText("Moved issue-1 to ready")).toBeTruthy();
    expect(state.execute).toHaveBeenCalledTimes(1);
  });

  it("enforces a denied policy", () => {
    writeAssistantActionPolicy("board.issue.transition", "deny");
    renderOffer();
    expect(screen.getByText(/blocked by settings/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Confirm action" })).toBeNull();
    expect(state.execute).not.toHaveBeenCalled();
  });

  it("auto-executes an explicitly requested action when configured", async () => {
    writeAssistantActionPolicy("board.issue.transition", "explicit");
    renderOffer();
    await waitFor(() => expect(state.execute).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Moved issue-1 to ready")).toBeTruthy();
  });
});
