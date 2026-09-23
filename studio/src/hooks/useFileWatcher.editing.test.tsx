// @vitest-environment jsdom
//
// The Source view's buffer is local to that component, so the store's
// _generation never moves and isDirty() cannot see an open edit. The
// watcher reads that to decide whether a file changed on disk may be
// reloaded automatically — and an auto-reload swaps the document AND the
// unit's revision, so the Apply that follows lands on top of whoever wrote
// the file meanwhile. The flag has to hold at BOTH sites that decide: the
// branch, and the 500 ms debounce that fires after it.
import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ openFile: vi.fn() }));
vi.mock("@/api/client", () => api);

const ws = vi.hoisted(() => {
  let handler: ((e: unknown) => void) | null = null;
  return {
    fileWatcher: {
      connect: vi.fn(),
      disconnect: vi.fn(),
      subscribe: vi.fn((fn: (e: unknown) => void) => {
        handler = fn;
        return () => {
          handler = null;
        };
      }),
    },
    emit: (e: unknown) => handler?.(e),
  };
});
vi.mock("@/api/ws", () => ({ fileWatcher: ws.fileWatcher }));

import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";
import { useFileWatcher } from "./useFileWatcher";
import { useUIStore } from "@/store/ui";

function Watcher() {
  useFileWatcher();
  return null;
}

function boundStore(): DocumentStore {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/demo/main.bot");
  store.getState().markSaved();
  return store;
}

function mount(store: DocumentStore) {
  return render(
    <DocumentStoreProvider store={store}>
      <Watcher />
    </DocumentStoreProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  useUIStore.setState({ toasts: [] });
  api.openFile.mockResolvedValue({
    source: "",
    document: createEmptyDocument(),
    diagnostics: [],
    path: "bots/demo/main.bot",
  });
});

afterEach(cleanup);

describe("the watcher against an open Source-view edit", () => {
  // The control, and it has to be here: without it every assertion below
  // would pass on a watcher that never reloads anything.
  it("reloads a clean buffer when the file changes on disk", async () => {
    mount(boundStore());
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1), { timeout: 2000 });
  });

  it("does not reload when an edit is already open", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({ path: "bots/demo/main.bot", rel: null, text: "x", base: "x",
      doc: null,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await new Promise((r) => setTimeout(r, 900));
    expect(api.openFile).not.toHaveBeenCalled();
  });

  // The site the first fix missed: the author opens the edit AFTER the
  // event arrives and before the debounce fires.
  it("does not reload when the edit opens inside the debounce window", async () => {
    const store = boundStore();
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await new Promise((r) => setTimeout(r, 100));
    store.getState().setSourceBuffer({ path: "bots/demo/main.bot", rel: null, text: "x", base: "x",
      doc: null,
    });
    await new Promise((r) => setTimeout(r, 900));
    expect(api.openFile).not.toHaveBeenCalled();
  });
});

// The third window, after the branch and the debounce: the author opens the
// edit while `openFile` is in flight. Applying the answer then swaps the
// document and the revision under what they are typing, and
// `applyOpenedFile` → `setCurrentFilePath` drops the buffer — so the text
// goes invisible to every discard path AND the next Apply is refused as
// stale, with the repair stranded.
describe("an edit opened while the reload is in flight", () => {
  it("is not applied over, and the change is offered instead", async () => {
    const store = boundStore();
    let release!: (v: unknown) => void;
    api.openFile.mockReturnValue(new Promise((r) => { release = r; }));
    mount(store);

    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));

    // The author starts typing while the answer is on the wire.
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "a repair the author typed",
      base: "workflow w:\n  entry: a\n",
      doc: null,
    });
    release({ path: "bots/demo/main.bot", document: createEmptyDocument(), source: "" });
    await new Promise((r) => setTimeout(r, 50));

    // The buffer is still there, and still visible to every discard path.
    expect(store.getState().sourceBuffer).not.toBeNull();
    expect(store.getState().hasUnsavedWork()).toBe(true);
    // …and the author is told, with a way to take the change when ready.
    const toasts = useUIStore.getState().toasts;
    expect(toasts.map((t) => t.message)).toContain("File changed externally");
    // The label says what the action takes: it applies the reload without a
    // second prompt, so a neutral "Reload" would be one click from the loss.
    expect(toasts[toasts.length - 1]?.action?.label).toBe("Reload and discard");
  });

  it("still applies a reload that lands on a clean buffer", async () => {
    const store = boundStore();
    let release!: (v: unknown) => void;
    api.openFile.mockReturnValue(new Promise((r) => { release = r; }));
    mount(store);

    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    await waitFor(() => expect(api.openFile).toHaveBeenCalledTimes(1));
    release({ path: "bots/demo/main.bot", document: createEmptyDocument(), source: "" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("File reloaded"),
    );
  });
});

// The FIRST window — the edit was already open when the external write
// landed — offers the same one-click reload. It applies without a second
// prompt, so its label owes the same sentence as the third window's.
describe("the toast raised when the edit was already open", () => {
  it("names what its action takes", async () => {
    const store = boundStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "a repair the author typed",
      base: "workflow w:\n  entry: a\n",
      doc: null,
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain(
        "File changed externally",
      ),
    );
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.action?.label).toBe("Reload and discard");
    expect(api.openFile).not.toHaveBeenCalled();
  });

  it("keeps the neutral label when only the document moved", async () => {
    const store = boundStore();
    store.getState().addAgent({
      name: "a",
      model: "m",
      input: "in",
      output: "out",
      system: "s",
      user: "u",
      session: "fresh",
    });
    mount(store);
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });

    await waitFor(() =>
      expect(useUIStore.getState().toasts.map((t) => t.message)).toContain(
        "File changed externally",
      ),
    );
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.action?.label).toBe("Reload");
  });
});
