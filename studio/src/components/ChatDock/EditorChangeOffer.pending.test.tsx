// @vitest-environment jsdom
//
// An assistant proposal applied to the editor changes the document without
// the author doing anything in that tab. While a file the author asked for
// is still opening there, it waits: landing first would get the author's
// Open refused for edits nobody made.
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
import { applyOpenedFile } from "@/lib/openedFile";
import { REPLACE_DEADLINE_MS, replaceDocument } from "@/lib/replaceDocument";
import { getOrCreateDocumentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import EditorChangeOffer from "./EditorChangeOffer";

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  window.history.replaceState({}, "", "/editor");
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  useServerInfoStore.setState({ info: null });
  useUIStore.setState({ toasts: [] });
  api.parseSource.mockResolvedValue({
    document: { ...createEmptyDocument(), workflows: [{ name: "changed", entry: "b", edges: [] }] },
    diagnostics: [],
    issues: [],
  });
  api.validate.mockResolvedValue({ diagnostics: [], warnings: [], issues: [] });
});

afterEach(cleanup);

async function captureProposalTab(policy: "explicit" | "ask" = "explicit") {
  const tabId = useTabsStore.getState().openTab("editor", { file: "bots/demo/main.bot" }, "demo");
  const store = getOrCreateDocumentStore(tabId);
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/demo/main.bot");
  store.getState().markSaved();
  api.unparse.mockResolvedValue({ source: "workflow original:\n  entry: a\n" });
  const snapshot = await captureActiveEditorDocument();
  if (!snapshot) throw new Error("no snapshot");
  proposal.current = {
    source: "workflow changed:\n  entry: b\n",
    sessionId: snapshot.sessionId,
    revision: snapshot.revision,
    applyIntent: "explicit",
    saveIntent: "none",
  };
  writeAssistantActionPolicy("editor.apply", policy);
  return store;
}

describe("an assistant proposal whose validation the author's Open overtook", () => {
  it("does not apply once validation answers", async () => {
    const store = await captureProposalTab();
    let landValidation!: (v: unknown) => void;
    api.validate.mockReturnValueOnce(new Promise((r) => (landValidation = r)));
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    await waitFor(() => expect(api.validate).toHaveBeenCalledTimes(1));
    void replaceDocument(store, "bots/other.bot", () => new Promise(() => {}), () => {});

    landValidation({ diagnostics: [], warnings: [], issues: [] });
    await new Promise((r) => setTimeout(r, 50));
    expect(store.getState().document?.workflows?.[0]?.name).not.toBe("changed");
  });
});

describe("an assistant proposal, while the author's Open is loading in its tab", () => {
  it("does not apply, and the Open lands", async () => {
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/demo/main.bot" }, "demo");
    const store = getOrCreateDocumentStore(tabId);
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/demo/main.bot");
    store.getState().markSaved();
    api.unparse.mockResolvedValue({ source: "workflow original:\n  entry: a\n" });
    const snapshot = await captureActiveEditorDocument();
    if (!snapshot) throw new Error("no snapshot");
    proposal.current = {
      source: "workflow changed:\n  entry: b\n",
      sessionId: snapshot.sessionId,
      revision: snapshot.revision,
      applyIntent: "explicit",
      saveIntent: "none",
    };
    // The policy that would apply it without a click.
    writeAssistantActionPolicy("editor.apply", "explicit");

    let land!: (v: unknown) => void;
    const outcome = replaceDocument(
      store,
      "bots/other.bot",
      () => new Promise((r) => (land = r)),
      (r, st) => applyOpenedFile(r as never, st),
    );
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    await new Promise((r) => setTimeout(r, 100));
    expect(api.parseSource).not.toHaveBeenCalled();
    expect(store.getState().document?.workflows?.[0]?.name).not.toBe("changed");

    land({ source: "O\n", document: createEmptyDocument(), diagnostics: [], path: "bots/other.bot" });
    expect(await outcome).toBe("applied");
    await waitFor(() => expect(store.getState().currentFilePath).toBe("bots/other.bot"));
    expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).not.toContain("was opening");
  });
});

describe("an assistant proposal, while an Open that then FAILS is loading in its tab", () => {
  it("waits without an error, and applies once the Open has ended", async () => {
    const store = await captureProposalTab();
    let failLoad!: (e: unknown) => void;
    const outcome = replaceDocument(
      store,
      "bots/gone.bot",
      () => new Promise((_, reject) => (failLoad = reject)),
      () => {},
    );
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    await new Promise((r) => setTimeout(r, 100));
    expect(api.parseSource).not.toHaveBeenCalled();
    expect(screen.queryByText(/A file is still opening/)).toBeNull();
    // Waiting, and saying so: a load has no timeout.
    expect(screen.getByText("Waiting for the file opening in that editor tab.")).toBeTruthy();

    // The Open fails: the tab holds the document the proposal was made on.
    failLoad(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
    await waitFor(() => expect(store.getState().document?.workflows?.[0]?.name).toBe("changed"));
  });
});

describe("an assistant proposal whose validation an Open overtook, when that Open then FAILS", () => {
  const overtakeThenFail = async (store: Awaited<ReturnType<typeof captureProposalTab>>, landValidation: (v: unknown) => void) => {
    let failLoad!: (e: unknown) => void;
    const outcome = replaceDocument(
      store,
      "bots/gone.bot",
      () => new Promise((_, reject) => (failLoad = reject)),
      () => {},
    );
    landValidation({ diagnostics: [], warnings: [], issues: [] });
    await new Promise((r) => setTimeout(r, 50));
    expect(store.getState().document?.workflows?.[0]?.name).not.toBe("changed");
    failLoad(new Error("404: file not found"));
    await expect(outcome).rejects.toThrow("404");
  };

  it("applies automatically once the Open has ended, and says nothing changed", async () => {
    const store = await captureProposalTab("explicit");
    let landValidation!: (v: unknown) => void;
    api.validate.mockReturnValueOnce(new Promise((r) => (landValidation = r)));
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    await waitFor(() => expect(api.validate).toHaveBeenCalledTimes(1));
    await overtakeThenFail(store, landValidation);

    await waitFor(() => expect(store.getState().document?.workflows?.[0]?.name).toBe("changed"));
    expect(api.validate).toHaveBeenCalledTimes(2);
    expect(screen.queryByText(/changed while the proposal was being validated/)).toBeNull();
  });

  it("gives the Apply button back once the Open has ended, and says nothing changed", async () => {
    const store = await captureProposalTab("ask");
    let landValidation!: (v: unknown) => void;
    api.validate.mockReturnValueOnce(new Promise((r) => (landValidation = r)));
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    fireEvent.click(await screen.findByRole("button", { name: "Apply to editor" }));
    await waitFor(() => expect(api.validate).toHaveBeenCalledTimes(1));
    await overtakeThenFail(store, landValidation);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByText(/changed while the proposal was being validated/)).toBeNull();
    fireEvent.click(apply);
    await waitFor(() => expect(store.getState().document?.workflows?.[0]?.name).toBe("changed"));
  });
});

describe("an assistant proposal waiting behind an Open the server never answers", () => {
  afterEach(() => vi.useRealTimers());

  it("applies once that Open's deadline has passed", async () => {
    const store = await captureProposalTab("explicit");
    vi.useFakeTimers();
    const hung = replaceDocument(store, "bots/hung.bot", () => new Promise<never>(() => {}), () => {}).catch(
      (err: unknown) => err,
    );
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(api.parseSource).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(REPLACE_DEADLINE_MS);
    expect(await hung).toBeInstanceOf(Error);
    vi.useRealTimers();
    await waitFor(() => expect(store.getState().document?.workflows?.[0]?.name).toBe("changed"));
  });
});
