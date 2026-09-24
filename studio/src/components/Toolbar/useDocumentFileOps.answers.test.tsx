// @vitest-environment jsdom
//
// Every file operation awaits the server, and its answer lands on the tab as
// it is THEN. The rule under test (#1770): an answer carries what it was
// asked about — the author's latest request, the document's generation, the
// Source view's text — and settles only that. A newer request wins quietly;
// work done meanwhile refuses the answer, out loud.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  parseSource: vi.fn(),
  unparse: vi.fn(),
  unparseUnitFile: vi.fn(),
  saveFile: vi.fn(),
  openFile: vi.fn(),
  loadExample: vi.fn(),
  validate: vi.fn(),
}));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  ...api,
}));
vi.mock("@/lib/monaco", () => ({
  default: ({ value, onChange }: { value?: string; onChange?: (v?: string) => void }) => (
    <textarea aria-label="source" value={value ?? ""} onChange={(e) => onChange?.(e.target.value)} />
  ),
}));
vi.mock("@/lib/iterLanguage", () => ({
  ITER_LANGUAGE_ID: "iter",
  iterLanguageConfig: {},
  iterTokensProvider: {},
}));
vi.mock("@/lib/iterMonacoCompletion", () => ({ registerIterCompletionProvider: vi.fn() }));

import { createEmptyDocument } from "@/lib/defaults";
import { applyOpenedFile } from "@/lib/openedFile";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";
import SourceView from "@/components/SourceView/SourceView";
import { useUIStore } from "@/store/ui";
import { useDocumentFileOps } from "./useDocumentFileOps";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const opened = (path: string, marker: string) => ({
  source: `${marker}\n`,
  document: { ...createEmptyDocument(), comments: [{ text: marker }] },
  diagnostics: [],
  path,
});

function Toolbarish() {
  const ops = useDocumentFileOps({ confirm: async () => true });
  return (
    <>
      <button onClick={() => void ops.handleNew()}>New</button>
      <button onClick={() => void ops.handlePickFile("file", "bots/a.bot")}>Open A</button>
      <button onClick={() => void ops.handlePickFile("file", "bots/b.bot")}>Open B</button>
      <button onClick={() => void ops.handlePickFile("file", "bots/gone.bot")}>Open gone</button>
      <button onClick={() => void ops.handleValidate()}>Validate</button>
      <button onClick={() => void ops.handleSave()}>Save</button>
      <input
        aria-label="import"
        type="file"
        onChange={(e) => void ops.handleImport(e)}
      />
    </>
  );
}

function tab(path = "bots/start.bot") {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath(path);
  store.getState().setCurrentSource("start\n");
  store.getState().markSaved();
  render(
    <DocumentStoreProvider store={store}>
      <SourceView />
      <Toolbarish />
    </DocumentStoreProvider>,
  );
  return store;
}

const marker = (store: ReturnType<typeof createDocumentStore>) =>
  store.getState().document?.comments?.[0]?.text ?? null;
const toasts = () => useUIStore.getState().toasts.map((t) => t.message).join(" | ");

beforeEach(() => {
  vi.resetAllMocks();
  api.unparse.mockResolvedValue({ source: "rendered\n" });
});

afterEach(() => {
  cleanup();
  useUIStore.setState({ toasts: [] });
});

describe("opening a file while another open is in flight", () => {
  it("lands the LAST request, whichever answer arrives first", async () => {
    for (const order of ["older-first", "newer-first"] as const) {
      cleanup();
      const store = tab();
      const a = deferred<ReturnType<typeof opened>>();
      const b = deferred<ReturnType<typeof opened>>();
      api.openFile.mockImplementation((path: string) => (path === "bots/a.bot" ? a.promise : b.promise));
      fireEvent.click(screen.getByRole("button", { name: "Open A" }));
      fireEvent.click(screen.getByRole("button", { name: "Open B" }));
      await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(2));

      await act(async () => {
        if (order === "older-first") {
          a.resolve(opened("bots/a.bot", "A"));
          await Promise.resolve();
          b.resolve(opened("bots/b.bot", "B"));
        } else {
          b.resolve(opened("bots/b.bot", "B"));
          await Promise.resolve();
          a.resolve(opened("bots/a.bot", "A"));
        }
      });
      await waitFor(() => expect(store.getState().currentFilePath).toBe("bots/b.bot"));
      expect(marker(store)).toBe("B");
      api.openFile.mockReset();
    }
  });

  it("does not land after File → New, which is a newer request", async () => {
    const store = tab();
    const a = deferred<ReturnType<typeof opened>>();
    api.openFile.mockReturnValue(a.promise);
    fireEvent.click(screen.getByRole("button", { name: "Open A" }));
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(store.getState().currentFilePath).toBeNull());

    await act(async () => a.resolve(opened("bots/a.bot", "A")));
    expect(store.getState().currentFilePath).toBeNull();
    expect(marker(store)).toBeNull();
    // Superseded, not refused: New is the author's newer request, not an
    // edit, so nothing claims "the editor changed while it was opening".
    expect(toasts()).not.toContain("was opening");
  });
});

describe("an answer that lands after the author worked", () => {
  it("does not open over Source text typed while the file loaded", async () => {
    const store = tab();
    const a = deferred<ReturnType<typeof opened>>();
    api.openFile.mockReturnValue(a.promise);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.click(screen.getByRole("button", { name: "Open A" }));
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    // The editor was open and clean when the confirm was asked, so it asked
    // nothing; the typing comes after.
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "typed while it loaded" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    await act(async () => a.resolve(opened("bots/a.bot", "A")));
    expect(store.getState().currentFilePath).toBe("bots/start.bot");
    expect(store.getState().sourceBuffer?.text).toBe("typed while it loaded");
    expect(toasts()).toContain("bots/a.bot was opening, so it was not opened");
  });

  it("does not import over canvas edits made while the file was read", async () => {
    const store = tab();
    const parsed = deferred<{ document: unknown; diagnostics: string[] }>();
    api.parseSource.mockReturnValue(parsed.promise);
    const file = new File(["dsl: 2\n"], "imported.bot", { type: "text/plain" });
    fireEvent.change(screen.getByLabelText("import"), { target: { files: [file] } });
    await waitFor(() => expect(api.parseSource).toHaveBeenCalledTimes(1));
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "canvas edit" }] });
    });

    await act(async () =>
      parsed.resolve({ document: { ...createEmptyDocument(), comments: [{ text: "IMPORTED" }] }, diagnostics: [] }),
    );
    expect(marker(store)).toBe("canvas edit");
    expect(store.getState().currentFilePath).toBe("bots/start.bot");
    expect(toasts()).toContain("imported.bot was opening, so it was not opened");
  });

  it("does not show diagnostics for a document that moved while it was validated", async () => {
    const store = tab();
    const result = deferred<{ diagnostics: string[]; warnings: string[]; issues: never[] }>();
    api.validate.mockReturnValue(result.promise);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() => expect(api.validate).toHaveBeenCalledTimes(1));
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "edited" }] });
    });

    await act(async () => result.resolve({ diagnostics: ["C999 stale"], warnings: [], issues: [] }));
    expect(store.getState().diagnostics).toEqual([]);
    expect(toasts()).toContain("The editor changed while validating");
  });
});

describe("a save that answers after the same file was reopened", () => {
  it("settles nothing on the reopened document", async () => {
    const store = tab("bots/a.bot");
    const save = deferred<{ path: string; source: string; revision?: string }>();
    api.saveFile.mockReturnValue(save.promise);
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "about to save" }] });
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));

    // Reopened from disk while the save is in flight: a new document under
    // the same name.
    api.openFile.mockResolvedValue(opened("bots/a.bot", "reopened"));
    fireEvent.click(screen.getByRole("button", { name: "Open A" }));
    await waitFor(() => expect(marker(store)).toBe("reopened"));
    expect(store.getState().currentSource).toBe("reopened\n");

    await act(async () => save.resolve({ path: "bots/a.bot", source: "the OLD save's source\n" }));
    expect(store.getState().currentSource).toBe("reopened\n");
    expect(marker(store)).toBe("reopened");

    // What is on screen was read before the write landed: saying "Saved"
    // alone would leave it there, marked saved, for the next save to write
    // back. A reload is offered instead.
    const offer = useUIStore
      .getState()
      .toasts.find((t) => t.message === "Saved bots/a.bot — the tab was reopened meanwhile and does not show what was saved.");
    expect(offer?.action?.label).toBe("Reload");
    api.openFile.mockResolvedValue(opened("bots/a.bot", "as saved"));
    await act(async () => offer?.action?.onClick());
    await waitFor(() => expect(marker(store)).toBe("as saved"));
  });

  it("says Saved, and offers nothing, when the reopen read what the save wrote", async () => {
    const store = tab("bots/a.bot");
    const save = deferred<{ path: string; source: string; revision?: string }>();
    api.saveFile.mockReturnValue(save.promise);
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "about to save" }] });
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));

    api.openFile.mockResolvedValue(opened("bots/a.bot", "as saved"));
    fireEvent.click(screen.getByRole("button", { name: "Open A" }));
    await waitFor(() => expect(marker(store)).toBe("as saved"));

    await act(async () => save.resolve({ path: "bots/a.bot", source: "as saved\n" }));
    expect(toasts()).toContain("Saved bots/a.bot");
    expect(toasts()).not.toContain("reopened meanwhile");
  });
});

describe("a save that answers after the watcher reloaded its file", () => {
  // Ctrl+S on a clean tab is a save too, and a clean tab is the one the
  // watcher reloads without asking.
  it("settles nothing on the reloaded document: its source and revision stay the disk's", async () => {
    const store = tab("bots/demo/main.bot");
    const files = [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }];
    act(() => {
      store.getState().setUnit({ root: "bots/demo", main: "main.bot", revision: "r1", files });
      store.getState().markSaved();
    });
    const save = deferred<{ path: string; source: string; revision?: string }>();
    api.saveFile.mockReturnValueOnce(save.promise);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));

    // What the watcher's automatic reload does with the file it read.
    act(() => {
      applyOpenedFile(
        { ...opened("bots/demo/main.bot", "reloaded"), unit: { root: "bots/demo", main: "main.bot", revision: "r9", files } },
        store.getState(),
      );
    });

    await act(async () => save.resolve({ path: "bots/demo/main.bot", source: "the save's source\n", revision: "r2" }));
    expect(store.getState().currentSource).toBe("reloaded\n");
    expect(store.getState().unit?.revision).toBe("r9");
    expect(marker(store)).toBe("reloaded");
    expect(toasts()).toContain("Saved bots/demo/main.bot");
  });
});

describe("a save while an Open that never lands is in flight", () => {
  it("settles the document it wrote: source, revision, saved mark", async () => {
    const store = tab("bots/demo/main.bot");
    act(() => {
      store.getState().setUnit({
        root: "bots/demo",
        main: "main.bot",
        revision: "r1",
        files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
      });
      store.getState().markSaved();
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "edit to save" }] });
    });
    const save = deferred<{ path: string; source: string; revision?: string }>();
    api.saveFile.mockReturnValueOnce(save.promise);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));

    // A recent file that is gone: asked for, never applied.
    api.openFile.mockRejectedValueOnce(new Error("404: file not found"));
    fireEvent.click(screen.getByRole("button", { name: "Open gone" }));
    await waitFor(() => expect(toasts()).toContain("Removed missing file from recents"));

    await act(async () => save.resolve({ path: "bots/demo/main.bot", source: "after\n", revision: "r2" }));
    expect(store.getState().unit?.revision).toBe("r2");
    expect(store.getState().currentSource).toBe("after\n");
    expect(store.getState().isDirty()).toBe(false);
  });
});

describe("an editor opened while a file loads, with nothing typed in it", () => {
  it("does not refuse the Open: there is no work it could take", async () => {
    const store = tab();
    await screen.findByRole("button", { name: "Edit" });
    const a = deferred<ReturnType<typeof opened>>();
    api.openFile.mockReturnValueOnce(a.promise);
    fireEvent.click(screen.getByRole("button", { name: "Open A" }));
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(store.getState().hasUnsavedWork()).toBe(false);

    await act(async () => a.resolve(opened("bots/a.bot", "A")));
    expect(store.getState().currentFilePath).toBe("bots/a.bot");
    expect(toasts()).not.toContain("was opening");
  });
});

describe("an Apply in flight, then an Open the author confirmed", () => {
  for (const order of ["apply-first", "open-first"] as const) {
    it(`opens the file and applies nothing, whichever answers first (${order})`, async () => {
      cleanup();
      const store = tab();
      fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
      fireEvent.change(screen.getByLabelText("source"), { target: { value: "text then discarded" } });
      await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));
      const parse = deferred<unknown>();
      api.parseSource.mockReturnValueOnce(parse.promise);
      fireEvent.click(screen.getByRole("button", { name: "Apply" }));
      await waitFor(() => expect(api.parseSource).toHaveBeenCalledTimes(1));
      const open = deferred<ReturnType<typeof opened>>();
      api.openFile.mockReturnValueOnce(open.promise);
      fireEvent.click(screen.getByRole("button", { name: "Open B" }));
      await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));

      const applied = { document: { ...createEmptyDocument(), comments: [{ text: "APPLIED" }] }, diagnostics: [] };
      await act(async () => {
        if (order === "apply-first") {
          parse.resolve(applied);
          await new Promise((r) => setTimeout(r, 0));
          open.resolve(opened("bots/b.bot", "B"));
        } else {
          open.resolve(opened("bots/b.bot", "B"));
          await new Promise((r) => setTimeout(r, 0));
          parse.resolve(applied);
        }
        await new Promise((r) => setTimeout(r, 0));
      });
      expect(store.getState().currentFilePath).toBe("bots/b.bot");
      expect(marker(store)).toBe("B");
      expect(toasts()).not.toContain("was opening");
      // Dropped quietly, whichever landed first: nothing about the old
      // edit is painted on the file that opened.
      expect(screen.queryByText(/while this was applying/)).toBeNull();
      api.parseSource.mockReset();
      api.openFile.mockReset();
    });
  }

  for (const order of ["apply-first", "open-first"] as const) {
    it(`applies nothing and keeps the text when that Open fails (${order})`, async () => {
      cleanup();
      const store = tab();
      fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
      fireEvent.change(screen.getByLabelText("source"), { target: { value: "text kept" } });
      await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));
      const parse = deferred<unknown>();
      api.parseSource.mockReturnValueOnce(parse.promise);
      fireEvent.click(screen.getByRole("button", { name: "Apply" }));
      await waitFor(() => expect(api.parseSource).toHaveBeenCalledTimes(1));
      const open = deferred<ReturnType<typeof opened>>();
      api.openFile.mockReturnValueOnce(open.promise);
      fireEvent.click(screen.getByRole("button", { name: "Open B" }));
      await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));

      const applied = { document: { ...createEmptyDocument(), comments: [{ text: "APPLIED" }] }, diagnostics: [] };
      await act(async () => {
        if (order === "apply-first") {
          parse.resolve(applied);
          await new Promise((r) => setTimeout(r, 0));
          open.reject(new Error("404: file not found"));
        } else {
          open.reject(new Error("404: file not found"));
          await new Promise((r) => setTimeout(r, 0));
          parse.resolve(applied);
        }
        await new Promise((r) => setTimeout(r, 0));
      });
      expect(store.getState().currentFilePath).toBe("bots/start.bot");
      expect(marker(store)).not.toBe("APPLIED");
      expect(store.getState().sourceBuffer?.text).toBe("text kept");
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe("text kept");
      api.parseSource.mockReset();
      api.openFile.mockReset();
    });
  }
});
