// @vitest-environment jsdom
//
// "Because the buffer is invisible to `isDirty()`, no `confirmDiscard()`
// anywhere can prompt for it" — #1662's first measured residual, from the
// side that does the taking. File → New is the cheapest of the paths that go
// through `confirmDiscard`; the same call guards Open, Import and Load
// example.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  saveFile: vi.fn(),
  openFile: vi.fn(),
  loadExample: vi.fn(),
  parseSource: vi.fn(),
  unparse: vi.fn(),
  validate: vi.fn(),
  listFiles: vi.fn(),
  parseBotSourceEditorPath: vi.fn(),
  inferCatalogBotId: vi.fn(),
}));
vi.mock("@/api/client", () => api);

import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";

import { useDocumentFileOps } from "./useDocumentFileOps";

const confirmed = vi.fn<() => Promise<boolean>>();

function Harness() {
  const ops = useDocumentFileOps({ confirm: confirmed });
  return <button onClick={() => void ops.handleNew()}>New</button>;
}

function mount(store: DocumentStore) {
  render(
    <DocumentStoreProvider store={store}>
      <Harness />
    </DocumentStoreProvider>,
  );
}

/** A tab with a saved document and an un-applied Source-view edit — the one
 *  state in which the document generation has not moved and the author still
 *  has work on screen. */
function storeWithOnlyASourceEdit(): DocumentStore {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/demo/main.bot");
  store.getState().markSaved();
  store.getState().setSourceBuffer({
    path: "bots/demo/main.bot",
    rel: null,
    text: "a repair the author typed",
    base: "workflow w:\n  entry: a\n",
      doc: null,
    });
  return store;
}

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  useUIStore.setState({ toasts: [] });
  api.parseBotSourceEditorPath.mockReturnValue(null);
  confirmed.mockResolvedValue(true);
});

afterEach(cleanup);

describe("a discard that would take an un-applied Source-view edit", () => {
  it("asks first, though the document itself is saved", async () => {
    const store = storeWithOnlyASourceEdit();
    expect(store.getState().isDirty()).toBe(false);
    mount(store);

    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(confirmed).toHaveBeenCalledTimes(1));
  });

  it("does not go through when the author declines", async () => {
    const store = storeWithOnlyASourceEdit();
    confirmed.mockResolvedValue(false);
    mount(store);

    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(confirmed).toHaveBeenCalledTimes(1));
    // The tab is still on its file: File → New would have detached it.
    expect(store.getState().currentFilePath).toBe("bots/demo/main.bot");
  });

  it("asks nothing when the buffer holds what its render produced", async () => {
    const store = storeWithOnlyASourceEdit();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "workflow w:\n  entry: a\n",
      base: "workflow w:\n  entry: a\n",
      doc: null,
    });
    mount(store);

    fireEvent.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() => expect(store.getState().currentFilePath).toBeNull());
    expect(confirmed).not.toHaveBeenCalled();
  });
});
