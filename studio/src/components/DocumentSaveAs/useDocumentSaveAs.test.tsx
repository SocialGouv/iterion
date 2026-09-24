// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ saveFile: vi.fn() }));
vi.mock("@/api/client", () => api);

import { createEmptyDocument } from "@/lib/defaults";
import { createDocumentStore, type DocumentStore } from "@/store/document";
import { applyOpenedFile } from "@/lib/openedFile";
import { replaceDocument } from "@/lib/replaceDocument";
import { useRecentsStore } from "@/store/recents";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import DocumentSaveAsDialog from "./DocumentSaveAsDialog";
import { useDocumentSaveAs } from "./useDocumentSaveAs";

function Harness({
  store,
  strict = false,
  current = true,
}: {
  store: DocumentStore;
  strict?: boolean;
  current?: boolean;
}) {
  const controller = useDocumentSaveAs();
  return (
    <>
      <button
        onClick={() =>
          controller.requestSaveAs({
            store,
            ...(strict ? { expectedGeneration: store.getState()._generation } : {}),
            isTargetCurrent: () => current,
          })
        }
      >
        Open Save As
      </button>
      <DocumentSaveAsDialog controller={controller} />
    </>
  );
}

function documentStore() {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().markSaved();
  return store;
}

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  useRecentsStore.setState({ recents: [] });
  useServerInfoStore.setState({ info: null });
  useUIStore.setState({ toasts: [] });
  api.saveFile.mockResolvedValue({
    path: "main.bot",
    source: "workflow main:\n  entry: agent_1\n",
  });
});

afterEach(cleanup);

describe("useDocumentSaveAs", () => {
  it("confirms a host-derived filename and binds only the successful response", async () => {
    const store = documentStore();
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    expect(await screen.findByDisplayValue("main.bot")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(store.getState().currentFilePath).toBe("main.bot"));
    expect(api.saveFile).toHaveBeenCalledWith(
      "main.bot",
      store.getState().document,
      { createOnly: true },
    );
    expect(store.getState().isDirty()).toBe(false);
    expect(useRecentsStore.getState().recents).toEqual(["main.bot"]);
  });

  it("keeps the dialog open after an atomic collision", async () => {
    const store = documentStore();
    api.saveFile.mockRejectedValue(new Error("API error 409: file already exists"));
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));

    expect((await screen.findByRole("alert")).textContent).toMatch(/already exists/i);
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(store.getState().currentFilePath).toBeNull();
  });

  it("cancels without persisting", async () => {
    const store = documentStore();
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  it("rejects a changed assistant-bound generation at confirmation", async () => {
    const store = documentStore();
    render(<Harness store={store} strict />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    await screen.findByRole("dialog");
    store.getState().setDocument(createEmptyDocument());
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect((await screen.findByRole("alert")).textContent).toMatch(/editor changed/i);
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  // Writing a salvage to a NEW file leaves the original whole, but hands the
  // author a copy missing the region the parser could not read, under the
  // name they chose — a loss they have no reason to suspect. The three sites
  // that write a document share this refusal.
  it("refuses a document the parser only salvaged", async () => {
    const store = documentStore();
    store.getState().setSalvaged(true);
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(api.saveFile).not.toHaveBeenCalled();
    const refused = useUIStore.getState().toasts;
    expect(refused[refused.length - 1]?.message).toMatch(/did not parse/i);
  });

  // The refusal at the dialog certifies the state the NAME was chosen in.
  // Typing a filename takes seconds, and an external write reaching the
  // watcher in that window swaps the document for a salvage — so the check
  // has to run again next to the write.
  it("refuses a document that became a salvage while the dialog was open", async () => {
    const store = documentStore();
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    await screen.findByRole("dialog");
    store.getState().setSalvaged(true);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect((await screen.findByRole("alert")).textContent).toMatch(/did not parse/i);
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  it("carries a whole file's un-applied Source text to the new name", async () => {
    const store = documentStore();
    store.setState({
      currentFilePath: "bots/demo/main.bot",
      sourceBuffer: {
        path: "bots/demo/main.bot",
        rel: null,
        text: "typed, not applied",
        base: "as rendered",
        doc: null,
        session: 1,
      },
    });
    render(<Harness store={store} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));

    await waitFor(() => expect(store.getState().currentFilePath).toBe("main.bot"));
    expect(store.getState().sourceBuffer).toMatchObject({
      path: "main.bot",
      rel: null,
      text: "typed, not applied",
    });
    expect(store.getState().hasUnsavedWork()).toBe(true);
  });

  it("does not carry a FRAGMENT's text into a tab no view can show it in, and says so", async () => {
    // A buffer typed for one file of a unit, with the unit gone: no writer
    // produces it today — every path that drops a unit drops the buffer in
    // the same `set` — so it is built by hand. Save As makes the tab a single
    // file, where the view shows the whole file only: carried, this text
    // could be neither adopted nor released, and the tab would read unsaved
    // for ever right after a successful save.
    const store = documentStore();
    store.setState({
      currentFilePath: "bots/demo/main.bot",
      sourceBuffer: {
        path: "bots/demo/main.bot",
        rel: "lib/nodes.bot",
        text: "typed for the fragment",
        base: "as rendered",
        doc: null,
        session: 1,
      },
    });
    render(<Harness store={store} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));

    await waitFor(() => expect(store.getState().currentFilePath).toBe("main.bot"));
    expect(store.getState().sourceBuffer).toBeNull();
    expect(store.getState().hasUnsavedWork()).toBe(false);
    const warning = useUIStore
      .getState()
      .toasts.find((t) => t.message.includes("lib/nodes.bot is no longer one of this tab's files"));
    expect(warning).toMatchObject({ type: "warning", persistent: true });
  });

  it("counts binding the new name as a replacement of what the tab is about", async () => {
    const store = documentStore();
    store.setState({ currentFilePath: "bots/demo/main.bot" });
    const at = store.getState()._replaced;
    render(<Harness store={store} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    await waitFor(() => expect(store.getState().currentFilePath).toBe("main.bot"));
    expect(store.getState()._replaced).toBe(at + 1);
  });

  it("is refused while an Open is still loading in the tab", async () => {
    // One replacement at a time: the Open is the author's latest request
    // for this tab.
    const store = documentStore();
    store.setState({ currentFilePath: "bots/demo/main.bot" });
    let resolveOpen!: (v: string) => void;
    const pendingOpen = replaceDocument(
      store,
      "b.bot",
      () => new Promise<string>((r) => (resolveOpen = r)),
      (path, s) => s.setCurrentFilePath(path),
    );
    render(<Harness store={store} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).toContain(
      "A file is still opening in this tab",
    );
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();

    resolveOpen("bots/b.bot");
    expect(await pendingOpen).toBe("applied");
    expect(store.getState().currentFilePath).toBe("bots/b.bot");
  });

  it("refuses at confirm when an Open started after the dialog opened", async () => {
    const store = documentStore();
    store.setState({ currentFilePath: "bots/demo/main.bot" });
    render(<Harness store={store} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    await screen.findByRole("button", { name: "Save" });
    let resolveOpen!: (v: string) => void;
    const pendingOpen = replaceDocument(
      store,
      "b.bot",
      () => new Promise<string>((r) => (resolveOpen = r)),
      (path, s) => s.setCurrentFilePath(path),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/A file is still opening in this tab/)).toBeTruthy();
    expect(api.saveFile).not.toHaveBeenCalled();
    resolveOpen("bots/b.bot");
    expect(await pendingOpen).toBe("applied");
  });

  it("does not bind the new name to a document an Open replaced while it wrote", async () => {
    const store = documentStore();
    store.setState({ currentFilePath: "bots/demo/main.bot" });
    let resolveSave!: (v: { path: string; source: string }) => void;
    api.saveFile.mockReturnValue(new Promise((r) => (resolveSave = r)));
    render(<Harness store={store} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));

    // An Open asked AFTER the Save As (the assistant can do it) lands first.
    await replaceDocument(store, "b.bot", async () => "bots/b.bot", (path, s) => {
      s.setDocument({ ...createEmptyDocument(), comments: [{ text: "B" }] });
      s.setCurrentFilePath(path);
    });
    resolveSave({ path: "main.bot", source: "workflow main:\n" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).toContain(
        "Saved main.bot, but the original editor session is no longer active",
      ),
    );
    expect(store.getState().currentFilePath).toBe("bots/b.bot");
    expect(store.getState().document?.comments?.[0]?.text).toBe("B");
  });

  it("does not bind the new name to the same file reloaded from disk while it wrote", async () => {
    // Same path, another document: what the watcher's automatic reload does
    // to a clean tab. Binding would give the new name to what was read from
    // the old file, not to what was written.
    const store = documentStore();
    store.setState({ currentFilePath: "bots/demo/main.bot" });
    let resolveSave!: (v: { path: string; source: string }) => void;
    api.saveFile.mockReturnValue(new Promise((r) => (resolveSave = r)));
    render(<Harness store={store} />);
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));

    act(() => {
      applyOpenedFile(
        {
          source: "reloaded\n",
          document: { ...createEmptyDocument(), comments: [{ text: "RELOADED" }] },
          diagnostics: [],
          path: "bots/demo/main.bot",
        },
        store.getState(),
      );
    });
    resolveSave({ path: "main.bot", source: "workflow main:\n" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).toContain(
        "Saved main.bot, but the original editor session is no longer active",
      ),
    );
    expect(store.getState().currentFilePath).toBe("bots/demo/main.bot");
    expect(store.getState().document?.comments?.[0]?.text).toBe("RELOADED");
    expect(store.getState().currentSource).toBe("reloaded\n");
    expect(store.getState().isDirty()).toBe(false);
  });

  it("does not open a filesystem Save As dialog in cloud mode", async () => {
    const store = documentStore();
    useServerInfoStore.setState({ info: { mode: "cloud" } as never });
    render(<Harness store={store} />);

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(api.saveFile).not.toHaveBeenCalled();
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.message).toMatch(/isn't available in cloud/i);
  });
});
