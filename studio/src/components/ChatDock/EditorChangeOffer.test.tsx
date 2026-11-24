// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  unparse: vi.fn(),
  parseSource: vi.fn(),
  parseUnitFile: vi.fn(),
  validate: vi.fn(),
  saveFile: vi.fn(),
  parseBotSourceEditorPath: vi.fn(() => null),
}));
const proposal = vi.hoisted(() => ({
  current: { source: null, sessionId: null, revision: null, file: null } as {
    source: string | null;
    sessionId: string | null;
    revision: number | null;
    file: string | null;
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
import { getOrCreateDocumentStore, SOURCE_MERGED } from "@/store/document";
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
  api.unparse.mockResolvedValue({ source: "workflow original:\n  entry: a\n" });
  const snapshot = await captureActiveEditorDocument();
  if (!snapshot) throw new Error("active editor snapshot was not captured");
  proposal.current = {
    source: "workflow changed:\n  entry: b\n",
    sessionId: snapshot.sessionId,
    revision: snapshot.revision,
    file: null,
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
    file: null,
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
  api.parseUnitFile.mockResolvedValue({
    document: { ...createEmptyDocument(), workflows: [{ name: "changed", entry: "b", edges: [] }] },
    diagnostics: [],
    bindable: true,
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
    // The digest rule: the save presents the digest the tab's open issued
    // (none in this fixture — the store's openedDigest is null — so the
    // options are absent and the server refuses an existing file's save).
    expect(api.saveFile).toHaveBeenCalledWith(
      "bots/demo/main.bot",
      store.getState().document,
      undefined,
    );
    expect(store.getState().isDirty()).toBe(false);
  });

  it("the save's answer refreshes the tab's write claim", async () => {
    const { store } = await liveProposal();
    store.getState().setOpenedDigest("sha-open");
    api.saveFile.mockResolvedValueOnce({
      path: "bots/demo/main.bot",
      source: "workflow changed:\n  entry: b\n",
      digest: "sha-saved",
    });
    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);
    await screen.findByText(/nothing has been written to disk/i);
    fireEvent.click(screen.getByRole("button", { name: "Save current file" }));
    await screen.findByText("Editor change saved");
    // The save presented the tab's digest; the store now holds what the
    // save answered, so the offer's second save presents that one.
    expect(api.saveFile).toHaveBeenCalledWith(
      "bots/demo/main.bot",
      expect.anything(),
      { digest: "sha-open" },
    );
    expect(store.getState().openedDigest).toBe("sha-saved");
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

  // #1647: the proposal targets ONE file of the unit — the Source view
  // picker's selection — applied through the unit's re-parse, never through
  // the whole-program parse that would fold every file into the main.
  it("applies a proposal to the file the Source view's picker names", async () => {
    const { store } = await liveProposal();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setSourceFile("lib/nodes.bot");

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);

    await screen.findByText(/nothing has been written to disk/i);
    expect(api.parseUnitFile).toHaveBeenCalledWith(
      "bots/demo/main.bot",
      "lib/nodes.bot",
      proposal.current.source,
    );
    expect(api.parseSource).not.toHaveBeenCalled();
    expect(store.getState().document?.workflows[0]?.name).toBe("changed");
  });

  it("defaults the target to the main, as the picker does before it renders", async () => {
    const { store } = await liveProposal();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);

    await waitFor(() =>
      expect(api.parseUnitFile).toHaveBeenCalledWith(
        "bots/demo/main.bot",
        "main.bot",
        proposal.current.source,
      ),
    );
  });

  it("disables Apply while the editor_file hint names another file than the picker", async () => {
    const { store } = await liveProposal();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setSourceFile("main.bot");
    proposal.current.file = "lib/nodes.bot";

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    expect((apply as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/switch the picker to lib\/nodes\.bot/i)).toBeTruthy();
    expect(api.parseUnitFile).not.toHaveBeenCalled();

    // The author clicks to switch, and the offer goes live on THAT file.
    act(() => store.getState().setSourceFile("lib/nodes.bot"));
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
  });

  it("disables Apply while the picker is on the merged program", async () => {
    const { store } = await liveProposal();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setSourceFile(SOURCE_MERGED);

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    expect((apply as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/merged program/i)).toBeTruthy();
    expect(api.parseUnitFile).not.toHaveBeenCalled();
  });

  it("saves a unit with the revision it was opened at, and keeps the one the save answers", async () => {
    const { store } = await liveProposal();
    const files = [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }];
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files,
    });
    api.saveFile.mockResolvedValue({
      path: "bots/demo/main.bot",
      source: "workflow changed:\n  entry: b\n",
      revision: "r2",
    });

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);
    await screen.findByText(/nothing has been written to disk/i);
    fireEvent.click(screen.getByRole("button", { name: "Save current file" }));
    await screen.findByText("Editor change saved");

    // Without the revision this save lands on saveUnit's "names no revision"
    // branch, which tells the author to reopen a file that is already open.
    expect(api.saveFile).toHaveBeenCalledWith("bots/demo/main.bot", expect.anything(), {
      revision: "r1",
      unitFiles: files,
    });
    expect(store.getState().unit?.revision).toBe("r2");
  });

  it("follows a unit that arrives after the proposal rendered", async () => {
    const { store } = await liveProposal();
    render(<EditorChangeOffer runId="run-1" revision={1} />);
    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));

    act(() => {
      store.getState().setUnit({ root: "bots/demo", main: "main.bot", revision: "r1", files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }] });
    });

    // Still applicable — the target defaults to the main, as the picker does.
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);
    await waitFor(() =>
      expect(api.parseUnitFile).toHaveBeenCalledWith(
        "bots/demo/main.bot",
        "main.bot",
        proposal.current.source,
      ),
    );
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
      file: null,
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

  // The buffer holds what the parser SALVAGED of a file it could not read
  // whole. Saving it puts that reading over what the author wrote — the same
  // refusal the toolbar's Save and Save As carry.
  it("refuses to save a buffer the parser only salvaged", async () => {
    const { store } = await liveProposal();
    store.getState().setSalvaged(true);
    // A save-only turn: nothing is applied, so the buffer written is the
    // salvage itself.
    proposal.current.source = null;
    proposal.current.saveIntent = "explicit";

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    fireEvent.click(await screen.findByRole("button", { name: "Save current file" }));

    expect(await screen.findByText(/did not parse/i)).toBeTruthy();
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  // And the way out: a proposal that parses whole makes the document the
  // program again, so the file Copi just repaired becomes writable.
  it("ends the salvage when a proposal parses whole", async () => {
    const { store } = await liveProposal();
    store.getState().setSalvaged(true);

    render(<EditorChangeOffer runId="run-1" revision={1} />);

    const apply = await screen.findByRole("button", { name: "Apply to editor" });
    await waitFor(() => expect((apply as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(apply);

    await waitFor(() => expect(store.getState().salvaged).toBe(false));
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
