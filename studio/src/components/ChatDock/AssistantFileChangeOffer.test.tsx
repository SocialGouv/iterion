// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const authoring = vi.hoisted(() => ({
  previewAssistantAuthoring: vi.fn(),
  commitAssistantAuthoring: vi.fn(),
  snapshotAssistantAuthoring: vi.fn(),
}));
const runs = vi.hoisted(() => ({ deliverHostEvent: vi.fn() }));
const proposal = vi.hoisted(() => ({
  current: {
    sessionId: "editor-session",
    revision: 7,
    intent: "explicit" as const,
    changes: [{
      scope: "workspace" as const,
      path: "scripts/helper.py",
      replacements: [{ before: "return 41", after: "return 42" }],
    }],
  },
}));
const session = vi.hoisted(() => ({
  store: {
    subscribe: () => () => {},
    getState: () => ({ _generation: 7, isDirty: false }),
  },
}));

vi.mock("@/api/assistantAuthoring", () => authoring);
vi.mock("@/api/runs", () => runs);
vi.mock("@/hooks/useFileChangeProposal", () => ({
  useFileChangeProposal: () => proposal.current,
}));
vi.mock("@/lib/chatDock/editorSession", () => ({
  resolveEditorSession: () => session,
  resolveAuthoringSnapshot: () => ({
    editor_path: "bots/demo/main.bot",
    files: [{
      scope: "workspace",
      path: "scripts/helper.py",
      available: true,
      readable: true,
      sha256: "abc",
      size: 10,
    }],
  }),
  isEditorSessionActive: () => true,
}));
vi.mock("@/store/ui", () => ({
  useUIStore: (selector: (state: { addToast: () => void }) => unknown) =>
    selector({ addToast: () => {} }),
}));

import AssistantFileChangeOffer from "./AssistantFileChangeOffer";

const savedFiles = [{
  scope: "workspace" as const,
  path: "scripts/helper.py",
  before: "return 41",
  after: "return 42",
}];

const freshAuthoring = {
  editor_path: "bots/demo/main.bot",
  files: [{
    scope: "workspace" as const,
    path: "scripts/new.json",
    size: 0,
    available: false,
    readable: false,
    is_manifest_declared: true,
    reason: "declared_missing_local_file",
  }],
};

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  window.history.replaceState({}, "", "/editor");
  authoring.previewAssistantAuthoring.mockResolvedValue({ files: savedFiles, saved: false });
  authoring.commitAssistantAuthoring.mockResolvedValue({ files: savedFiles, saved: true });
  authoring.snapshotAssistantAuthoring.mockResolvedValue(freshAuthoring);
  runs.deliverHostEvent.mockResolvedValue({ delivered: true });
});

afterEach(cleanup);

async function saveChanges(runId = "assistant-run") {
  render(<AssistantFileChangeOffer runId={runId} revision={1} />);
  fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));
  fireEvent.click(await screen.findByRole("button", { name: "Close" }));
  fireEvent.click(await screen.findByRole("button", { name: "Save authoring changes" }));
}

describe("AssistantFileChangeOffer", () => {
  it("wakes Copi with a bounded completion receipt after saving", async () => {
    await saveChanges("assistant-success");

    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledWith(
      "assistant-success",
      "action-completed",
      {
        action: "editor.files.save",
        args: {
          editor_session_id: "editor-session",
          editor_revision: 7,
          files: [{
            scope: "workspace",
            path: "scripts/helper.py",
            operation: "replace",
          }],
          authoring: freshAuthoring,
        },
        message: "Saved 1 authoring file change",
      },
    ));
  });

  it("wakes Copi with a bounded repair perimeter when preview fails", async () => {
    authoring.previewAssistantAuthoring.mockRejectedValueOnce(new Error(
      "changes[1].replacements[0]: before text matched 2 times, want exactly once\nsource-secret",
    ));
    render(<AssistantFileChangeOffer runId="assistant-preview" revision={1} />);
    fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));

    expect(await screen.findByText(/source-secret/)).toBeTruthy();
    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledWith(
      "assistant-preview",
      "action-failed",
      {
        action: "editor.files.save",
        phase: "preview",
        attempt: 1,
        message: "changes[1].replacements[0]: before text matched 2 times, want exactly once",
        args: {
          editor_session_id: "editor-session",
          editor_revision: 7,
          editor_path: "bots/demo/main.bot",
          files: [{
            scope: "workspace",
            path: "scripts/helper.py",
            available: true,
            readable: true,
          }],
        },
      },
    ));
    const receipt = runs.deliverHostEvent.mock.calls[0]?.[2] as { message?: string } | undefined;
    expect(receipt?.message).not.toContain("source-secret");
  });

  it("wakes Copi when saving fails", async () => {
    authoring.commitAssistantAuthoring.mockRejectedValueOnce(new Error("stale file"));
    await saveChanges("assistant-save-failure");

    expect(await screen.findByText("stale file")).toBeTruthy();
    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledWith(
      "assistant-save-failure",
      "action-failed",
      expect.objectContaining({ phase: "save", attempt: 1 }),
    ));
  });

  it("keeps a successful save successful when the receipt races the chat boundary", async () => {
    runs.deliverHostEvent.mockRejectedValueOnce(new Error("assistant is not at a chat boundary"));
    await saveChanges();

    expect(await screen.findByText("Authoring file changes saved")).toBeTruthy();
    expect(authoring.commitAssistantAuthoring).toHaveBeenCalledTimes(1);
  });

  it("retries only a chat-boundary conflict while delivering a failure receipt", async () => {
    authoring.previewAssistantAuthoring.mockRejectedValueOnce(new Error("stale file"));
    const { ApiError } = await import("@/api/client");
    runs.deliverHostEvent
      .mockRejectedValueOnce(new ApiError(409, "assistant is not at a chat boundary"))
      .mockResolvedValueOnce({ delivered: true });
    render(<AssistantFileChangeOffer runId="assistant-retry" revision={1} />);
    fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));

    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledTimes(2), { timeout: 1_000 });
  });

  it("caps failure receipts after two delivered recovery turns", async () => {
    authoring.previewAssistantAuthoring.mockRejectedValue(new Error("stale file"));
    render(<AssistantFileChangeOffer runId="assistant-cap" revision={1} />);
    const review = await screen.findByRole("button", { name: "Review changes" });
    fireEvent.click(review);
    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledTimes(1));
    fireEvent.click(review);
    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledTimes(2));
    fireEvent.click(review);

    expect(await screen.findByText(/Copi was not notified after two automatic recovery attempts/)).toBeTruthy();
    expect(runs.deliverHostEvent).toHaveBeenCalledTimes(2);
  });

  it("clears a failure cap after a successful save", async () => {
    authoring.previewAssistantAuthoring.mockRejectedValueOnce(new Error("stale file"));
    render(<AssistantFileChangeOffer runId="assistant-clear" revision={1} />);
    fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));
    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledTimes(1));

    authoring.previewAssistantAuthoring.mockResolvedValueOnce({ files: savedFiles, saved: false });
    fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));
    fireEvent.click(await screen.findByRole("button", { name: "Close" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save authoring changes" }));
    await screen.findByText("Authoring file changes saved");

    cleanup();
    authoring.previewAssistantAuthoring.mockRejectedValueOnce(new Error("stale file"));
    render(<AssistantFileChangeOffer runId="assistant-clear" revision={1} />);
    fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));
    await waitFor(() => expect(runs.deliverHostEvent).toHaveBeenCalledTimes(3));
    expect(runs.deliverHostEvent.mock.calls[2]?.[2]).toMatchObject({ attempt: 1 });
  });

  it("does not deliver a failure receipt without a conversational run", async () => {
    authoring.previewAssistantAuthoring.mockRejectedValueOnce(new Error("stale file"));
    render(<AssistantFileChangeOffer runId={null} revision={1} />);
    fireEvent.click(await screen.findByRole("button", { name: "Review changes" }));

    expect(await screen.findByText("stale file")).toBeTruthy();
    expect(runs.deliverHostEvent).not.toHaveBeenCalled();
  });
});
