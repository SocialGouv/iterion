// @vitest-environment jsdom
//
// The toolbar's Save is one of the three sites that write a document. A
// document the parser only SALVAGED — the file minus the region it could not
// read — must not reach the disk under the file's own name: that is the loss
// #1251 is about, and the one the other two writers refuse the same way.
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

function Harness() {
  const ops = useDocumentFileOps({ confirm: async () => true });
  return (
    <button onClick={() => void ops.handleSave()}>Save</button>
  );
}

function salvagedStore(path: string): DocumentStore {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath(path);
  store.getState().setSalvaged(true);
  store.getState().markSaved();
  return store;
}

beforeEach(() => {
  vi.resetAllMocks();
  window.localStorage.clear();
  useUIStore.setState({ toasts: [] });
  api.parseBotSourceEditorPath.mockReturnValue(null);
  api.saveFile.mockResolvedValue({ path: "bots/x/main.bot", source: "" });
});

afterEach(cleanup);

describe("the toolbar's Save", () => {
  it("refuses a document the parser only salvaged, and says where to repair it", async () => {
    const store = salvagedStore("bots/x/main.bot");
    render(
      <DocumentStoreProvider store={store}>
        <Harness />
      </DocumentStoreProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(useUIStore.getState().toasts.length).toBeGreaterThan(0));
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.message).toMatch(/did not parse/i);
    expect(toasts[toasts.length - 1]?.message).toMatch(/Source view/i);
    expect(api.saveFile).not.toHaveBeenCalled();
  });

  it("writes one that is not a salvage", async () => {
    const store = salvagedStore("bots/x/main.bot");
    store.getState().setSalvaged(false);
    render(
      <DocumentStoreProvider store={store}>
        <Harness />
      </DocumentStoreProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(api.saveFile).toHaveBeenCalled());
    expect(api.saveFile.mock.calls[0]?.[0]).toBe("bots/x/main.bot");
  });
});
