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
  },
}));

vi.mock("@/api/client", () => api);
vi.mock("@/hooks/useEditorProposal", () => ({
  useEditorProposal: () => proposal.current,
}));

import { createEmptyDocument } from "@/lib/defaults";
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
  };
  return { tabId, store, snapshot };
}

beforeEach(() => {
  vi.resetAllMocks();
  proposal.current = { source: null, sessionId: null, revision: null };
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

  it("refuses to affect another active editor tab", async () => {
    await liveProposal();
    useTabsStore.getState().newEditorTab("other");

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    expect((apply as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/return to the editor tab/i)).toBeTruthy();
  });

  it("keeps Save As under operator control for an untitled buffer", async () => {
    await liveProposal(null);
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);
    await screen.findByText(/use save as in the editor/i);

    expect(
      (screen.getByRole("button", { name: "Save current file" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expect(api.saveFile).not.toHaveBeenCalled();
  });
});
