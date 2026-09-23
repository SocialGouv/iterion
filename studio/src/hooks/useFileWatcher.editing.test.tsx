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
    store.getState().setSourceBuffer({ path: "bots/demo/main.bot", rel: null, text: "x", base: "x" });
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
    store.getState().setSourceBuffer({ path: "bots/demo/main.bot", rel: null, text: "x", base: "x" });
    await new Promise((r) => setTimeout(r, 900));
    expect(api.openFile).not.toHaveBeenCalled();
  });
});
