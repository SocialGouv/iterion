// @vitest-environment jsdom
//
// A cloud bot the SERVER's read budget refused whole opens as a salvage
// (read_refusal): the editor holds nothing of the file. Copi is not handed
// that salvage as the file — the document is withheld and the refusal said —
// and a proposal applied to the tab does not end the salvage: the save stays
// refused, so nothing written from what nobody read reaches the stored file.

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  unparse: vi.fn(),
  parseSource: vi.fn(),
  validate: vi.fn(),
  saveFile: vi.fn(),
  parseBotSourceEditorPath: vi.fn(),
  getFileDependencyMetadata: vi.fn(),
}));
const authoring = vi.hoisted(() => ({ snapshotAssistantAuthoring: vi.fn() }));
const proposal = vi.hoisted(() => ({
  current: { source: null, sessionId: null, revision: null, applyIntent: "none", saveIntent: "none" } as {
    source: string | null;
    sessionId: string | null;
    revision: number | null;
    applyIntent: "none" | "suggested" | "explicit";
    saveIntent: "none" | "suggested" | "explicit";
  },
}));
vi.mock("@/api/client", () => api);
vi.mock("@/api/assistantAuthoring", () => authoring);
vi.mock("@/hooks/useEditorProposal", () => ({ useEditorProposal: () => proposal.current }));

import parsedPastTheBudget from "@/lib/__fixtures__/read-refusal-parse.json";
import type { IterDocument, ReadRefusal } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import { applyOpenedFile } from "@/lib/openedFile";
import { salvageRefusal } from "@/lib/salvage";
import { captureActiveEditorDocument } from "@/lib/chatDock/editorSession";
import { getOrCreateDocumentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";
import EditorChangeOffer from "@/components/ChatDock/EditorChangeOffer";

const PATH = "botsource://t1/big/main.bot";
const refusal = parsedPastTheBudget.read_refusal as ReadRefusal;

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  window.history.replaceState({}, "", "/editor");
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  useServerInfoStore.setState({ info: null });
  authoring.snapshotAssistantAuthoring.mockRejectedValue(new Error("authoring snapshot past its byte limit"));
  api.parseBotSourceEditorPath.mockImplementation((p: string) =>
    p.startsWith("botsource://") ? { teamID: "t1", slug: "big", rel: "main.bot" } : null,
  );
  api.validate.mockResolvedValue({ diagnostics: [], warnings: [], issues: [] });
  api.unparse.mockResolvedValue({ source: "" });
  api.saveFile.mockResolvedValue({ path: PATH, source: "workflow rebuilt:\n  entry: a\n" });
});

afterEach(cleanup);

function openPastTheBudget() {
  const tabId = useTabsStore.getState().openTab("editor", { file: PATH }, "big");
  const store = getOrCreateDocumentStore(tabId);
  // The cloud editor's open of a main past the server's budget: /api/parse's
  // own answer (pkg/server fixture), the stored text as the source.
  applyOpenedFile(
    {
      source: "workflow big:\n  entry: a\n" + "prompt p:\n  a line the studio never read\n".repeat(50),
      document: parsedPastTheBudget.document as unknown as IterDocument,
      diagnostics: parsedPastTheBudget.diagnostics,
      path: PATH,
      bindable: false,
      read_refusal: refusal,
    },
    store.getState(),
  );
  expect(store.getState().salvaged).toBe(true);
  return store;
}

describe("Copi over a salvage the server's reader made", () => {
  it("is not handed the salvage as the document: withheld, the refusal said", async () => {
    openPastTheBudget();
    const snapshot = await captureActiveEditorDocument();
    if (!snapshot) throw new Error("no snapshot");
    expect(snapshot).toMatchObject({ file: PATH, complete: false, dirty: false, readRefusal: refusal });
    expect(snapshot.source).toBeUndefined();
    expect(snapshot.readRefusal?.hint ?? "").toContain("ITERION_DSL_READ_BUDGET");
    // Nothing of the salvage was serialised to send.
    expect(api.unparse).not.toHaveBeenCalled();
  });

  it("applies a whole proposal to the canvas, and the salvage survives it: the save is refused", async () => {
    const store = openPastTheBudget();
    const snapshot = await captureActiveEditorDocument();
    if (!snapshot) throw new Error("no snapshot");
    proposal.current = {
      source: "workflow rebuilt:\n  entry: a\n",
      sessionId: snapshot.sessionId,
      revision: snapshot.revision,
      applyIntent: "explicit",
      saveIntent: "none",
    };
    api.parseSource.mockResolvedValue({
      document: { ...createEmptyDocument(), workflows: [{ name: "rebuilt", entry: "a", edges: [] }] },
      diagnostics: [],
      issues: [],
      bindable: true,
    });
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);
    await screen.findByText(/nothing has been written to disk/i);

    // The canvas holds the proposal; the salvage and its refusal stay.
    expect(store.getState().document?.workflows?.[0]?.name).toBe("rebuilt");
    expect(store.getState().salvaged).toBe(true);
    expect(store.getState().readRefusal).toEqual(refusal);
    expect(salvageRefusal(store.getState())).toContain("ITERION_DSL_READ_BUDGET");

    fireEvent.click(screen.getByRole("button", { name: "Save current file" }));
    await waitFor(() => expect(document.body.textContent ?? "").toContain("ITERION_DSL_READ_BUDGET"));
    expect(api.saveFile).not.toHaveBeenCalled();
  });
});
