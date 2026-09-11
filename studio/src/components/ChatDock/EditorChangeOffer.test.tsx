// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  unparse: vi.fn(),
  parseSource: vi.fn(),
  validate: vi.fn(),
  saveFile: vi.fn(),
  parseBotSourceEditorPath: vi.fn(() => null),
}));
const proposal = vi.hoisted(() => ({
  current: { source: null, sessionId: null, revision: null } as {
    source: string | null;
    sessionId: string | null;
    revision: number | null;
    applyIntent: "none" | "suggested" | "explicit";
    saveIntent: "none" | "suggested" | "explicit";
  },
}));

vi.mock("@/api/client", () => api);
vi.mock("@/hooks/useEditorProposal", () => ({
  useEditorProposal: () => proposal.current,
}));

import { createEmptyDocument } from "@/lib/defaults";
import { writeAssistantActionPolicy } from "@/lib/chatDock/assistantActions";
import { captureActiveEditorDocument } from "@/lib/chatDock/editorSession";
import { getOrCreateDocumentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";

import EditorChangeOffer from "./EditorChangeOffer";

async function liveProposal(path: string | null = "bots/demo/main.bot") {
  const tabId = useTabsStore
    .getState()
    .openTab("editor", path ? { file: path } : {}, "demo");
  const store = getOrCreateDocumentStore(tabId);
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath(path);
  store.getState().markSaved();
  api.unparse.mockResolvedValue("workflow original:\n  entry: a\n");
  const snapshot = await captureActiveEditorDocument();
  if (!snapshot) throw new Error("active editor snapshot was not captured");
  proposal.current = {
    source: "workflow changed:\n  entry: b\n",
    sessionId: snapshot.sessionId,
    revision: snapshot.revision,
    applyIntent: "explicit",
    saveIntent: "none",
  };
  return { tabId, store, snapshot };
}

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  window.history.replaceState({}, "", "/editor");
  proposal.current = {
    source: null,
    sessionId: null,
    revision: null,
    applyIntent: "none",
    saveIntent: "none",
  };
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  useServerInfoStore.setState({ info: null });
  api.parseSource.mockResolvedValue({
    document: { ...createEmptyDocument(), workflows: [{ name: "changed", entry: "b", edges: [] }] },
    diagnostics: [],
    issues: [],
  });
  api.validate.mockResolvedValue({ diagnostics: [], warnings: [], issues: [] });
  api.saveFile.mockResolvedValue({
    path: "bots/demo/main.bot",
    source: "workflow changed:\n  entry: b\n",
  });
});

afterEach(cleanup);

describe("EditorChangeOffer", () => {
  it("applies to the live buffer first, then saves only on the second click", async () => {
    const { store } = await liveProposal();
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);

    await screen.findByText(/nothing has been written to disk/i);
    expect(api.saveFile).not.toHaveBeenCalled();
    expect(store.getState().document?.workflows[0]?.name).toBe("changed");
    expect(store.getState().isDirty()).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Save current file" }));
    await screen.findByText("Editor change saved");
    expect(api.saveFile).toHaveBeenCalledWith(
      "bots/demo/main.bot",
      store.getState().document,
    );
    expect(store.getState().isDirty()).toBe(false);
  });

  it("refuses a proposal after the operator changed the captured document", async () => {
    const { store } = await liveProposal();
    store.getState().setDocument(createEmptyDocument());

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    expect((apply as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/document changed since/i)).toBeTruthy();
    expect(api.parseSource).not.toHaveBeenCalled();
  });

  it("refuses to apply a proposal to a materialized shared bundle", async () => {
    await liveProposal(".botz/shared-planner/main.bot");

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    expect((apply as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/locked shared bundle is read-only/i)).toBeTruthy();
    expect(api.parseSource).not.toHaveBeenCalled();
  });

  it("refuses to affect another active editor tab", async () => {
    await liveProposal();
    useTabsStore.getState().newEditorTab("other");

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    expect((apply as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/return to the captured editor tab/i)).toBeTruthy();
  });

  it("opens the shared Save As flow for an applied untitled buffer", async () => {
    const { store } = await liveProposal(null);
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);
    await screen.findByText(/choose a location to write it to disk/i);

    fireEvent.click(screen.getByRole("button", { name: "Choose location and save" }));
    await screen.findByRole("dialog");
    expect(api.saveFile).not.toHaveBeenCalled();

    fireEvent.change(screen.getByPlaceholderText("filename.bot"), {
      target: { value: "reviewed-workflow.bot" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Editor change saved");

    expect(api.saveFile).toHaveBeenCalledWith(
      "reviewed-workflow.bot",
      store.getState().document,
      { createOnly: true },
    );
    expect(store.getState().currentFilePath).toBe("bots/demo/main.bot");
  });

  it("saves the host buffer without applying model content on a save-only turn", async () => {
    const { store, snapshot } = await liveProposal(null);
    proposal.current = {
      source: null,
      sessionId: snapshot.sessionId,
      revision: snapshot.revision,
      applyIntent: "explicit",
      saveIntent: "explicit",
    };
    api.saveFile.mockResolvedValue({
      path: "host-choice.bot",
      source: "workflow original:\n  entry: a\n",
    });

    render(<EditorChangeOffer runId="run-1" revision={2} />);

    expect(await screen.findByText("Save editor buffer")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Apply to editor" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Choose location and save" }));
    await screen.findByRole("dialog");
    fireEvent.change(screen.getByPlaceholderText("filename.bot"), {
      target: { value: "host-choice.bot" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Editor change saved");

    expect(api.parseSource).not.toHaveBeenCalled();
    expect(api.saveFile).toHaveBeenCalledWith(
      "host-choice.bot",
      store.getState().document,
      { createOnly: true },
    );
  });

  it("offers one confirmed apply-and-save action", async () => {
    const { store } = await liveProposal();
    proposal.current.saveIntent = "explicit";
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    fireEvent.click(await screen.findByRole("button", { name: "Apply and save" }));
    await screen.findByText("Editor change saved");

    expect(store.getState().document?.workflows[0]?.name).toBe("changed");
    expect(api.saveFile).toHaveBeenCalledTimes(1);
    expect(store.getState().isDirty()).toBe(false);
  });

  it("applies an untitled draft before asking where to save it", async () => {
    const { store } = await liveProposal(null);
    proposal.current.saveIntent = "explicit";
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    fireEvent.click(await screen.findByRole("button", { name: "Apply and save" }));
    await screen.findByRole("dialog");

    expect(store.getState().document?.workflows[0]?.name).toBe("changed");
    expect(api.parseSource).toHaveBeenCalledTimes(1);
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  it("rechecks save policy when an untitled destination is confirmed", async () => {
    await liveProposal(null);
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    fireEvent.click(await screen.findByRole("button", { name: "Apply to editor" }));
    fireEvent.click(await screen.findByRole("button", { name: "Choose location and save" }));
    await screen.findByRole("dialog");
    writeAssistantActionPolicy("editor.save", "deny");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect((await screen.findByRole("alert")).textContent).toMatch(/saving assistant changes is disabled/i);
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  it("auto-applies and saves only when both policies and explicit intent allow it", async () => {
    const { store } = await liveProposal();
    proposal.current.saveIntent = "explicit";
    writeAssistantActionPolicy("editor.apply", "explicit");
    writeAssistantActionPolicy("editor.save", "explicit");

    render(<EditorChangeOffer runId="run-1" revision={1} />);
    await screen.findByText("Editor change saved");

    expect(api.parseSource).toHaveBeenCalledTimes(1);
    expect(api.saveFile).toHaveBeenCalledTimes(1);
    expect(store.getState().isDirty()).toBe(false);
  });

  it("enforces a denied apply policy instead of merely hiding the setting", async () => {
    await liveProposal();
    writeAssistantActionPolicy("editor.apply", "deny");
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    expect(screen.getByText(/applying assistant changes is disabled/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Apply to editor" })).toBeNull();
    expect(api.parseSource).not.toHaveBeenCalled();
  });

  it("keeps the proposal but requires returning to the editor surface", async () => {
    await liveProposal();
    window.history.replaceState({}, "", "/runs/run-2");
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    expect(screen.getByText(/return to the captured editor tab/i)).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: "Apply to editor" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Return to the bot" }));
    await waitFor(() => expect(window.location.pathname).toBe("/editor"));
  });
});
