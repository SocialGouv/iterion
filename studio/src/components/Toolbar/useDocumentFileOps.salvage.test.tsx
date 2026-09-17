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
    <>
      <button onClick={() => void ops.handleSave()}>Save</button>
      <button onClick={() => void ops.handleDownload()}>Download</button>
      <button onClick={() => void ops.handleCopySource()}>Copy</button>
      <input
        aria-label="import"
        type="file"
        onChange={(e) => void ops.handleImport(e)}
      />
    </>
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

  // Import unbinds, and unbinding clears the flag — which looks like
  // protection and is the opposite: an unbound buffer still has one write,
  // Save As, and it would put a file missing what the parser could not read
  // under the name the author chose.
  it("marks an imported file the parser could not read whole a salvage", async () => {
    const store = createDocumentStore();
    api.parseSource.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: ["imported.bot:4:1: error [E012]: unknown property"],
      bindable: false,
    });
    render(
      <DocumentStoreProvider store={store}>
        <Harness />
      </DocumentStoreProvider>,
    );

    const file = new File(["workflow y:\n  entry: done\n\nagent broken\n  nope\n"], "imported.bot");
    fireEvent.change(screen.getByLabelText("import"), { target: { files: [file] } });

    await waitFor(() => expect(api.parseSource).toHaveBeenCalled());
    await waitFor(() => expect(store.getState().salvaged).toBe(true));
    expect(store.getState().currentFilePath).toBeNull();
  });

  // Download and Copy hand the document out AS the program: a .bot on the
  // author's disk under a name they trust, or text they will paste into one.
  // Same harm as a save, so the same refusal — the class is every site that
  // materialises the document, not only the ones that write to the workspace.
  it.each([
    ["Download", "downloads"],
    ["Copy", "copies"],
  ])("refuses to export a salvage when the author %s it", async (button) => {
    const store = salvagedStore("bots/x/main.bot");
    api.unparse.mockResolvedValue("workflow x:\n  entry: done\n");
    render(
      <DocumentStoreProvider store={store}>
        <Harness />
      </DocumentStoreProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: button }));

    await waitFor(() => expect(useUIStore.getState().toasts.length).toBeGreaterThan(0));
    const toasts = useUIStore.getState().toasts;
    expect(toasts[toasts.length - 1]?.message).toMatch(/did not parse/i);
    expect(api.unparse).not.toHaveBeenCalled();
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
