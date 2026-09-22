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
    api.unparse.mockResolvedValue({ source: "workflow x:\n  entry: done\n" });
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

// The writer cannot reproduce every `.bot`: a value its author wrote over
// several lines has no multi-line form and would come back as one (#1612,
// pkg/dsl/canon). The server refuses to write such a file and says which
// file, which line, and what to do — and these are the surfaces that have
// to pass that on rather than swallow it.
describe("the toolbar against a file the writer cannot reproduce", () => {
  function boundStore(path: string): DocumentStore {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath(path);
    store.getState().markSaved();
    return store;
  }

  it("shows the server's reason when a save is refused, not four words", async () => {
    const store = boundStore("bots/x/main.bot");
    api.saveFile.mockRejectedValue(
      new Error(
        "bots/x/main.bot cannot be saved from the studio: the value at line 5 is written over several lines and the writer has no form for one — its 470494 characters would come back as a single line (#1612). Leave the file as it is, or edit it directly",
      ),
    );
    render(
      <DocumentStoreProvider store={store}>
        <Harness />
      </DocumentStoreProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    // The forbidden alternative, named: "Save failed" alone, which leaves
    // the author with nothing to act on.
    await waitFor(() => {
      const texts = useUIStore.getState().toasts.map((t) => t.message);
      expect(texts.join("\n")).toMatch(/line 5/);
    });
    expect(useUIStore.getState().toasts.map((t) => t.message).join("\n")).toMatch(/#1612/);
  });

  // Download is the harsher of the two: a `.bot` on the author's disk,
  // under a name they will trust. It was the one of the pair with no
  // witness — deleting its refusal left the whole suite green.
  it("does not write a .bot the file does not contain", async () => {
    const store = boundStore("bots/x/main.bot");
    api.unparse.mockResolvedValue({
      source: "THE FOLDED RENDER",
      refused: "the value at line 5 is written over several lines",
    });
    const created: string[] = [];
    const realCreate = URL.createObjectURL;
    URL.createObjectURL = vi.fn(() => {
      created.push("blob");
      return "blob:x";
    }) as unknown as typeof URL.createObjectURL;
    try {
      render(
        <DocumentStoreProvider store={store}>
          <Harness />
        </DocumentStoreProvider>,
      );
      fireEvent.click(screen.getByRole("button", { name: "Download" }));
      await waitFor(() => {
        const texts = useUIStore.getState().toasts.map((t) => t.message).join("\n");
        expect(texts).toMatch(/cannot be downloaded/i);
      });
      // The forbidden alternative, named: a file actually written.
      expect(created).toHaveLength(0);
    } finally {
      URL.createObjectURL = realCreate;
    }
  });

  it("warns instead of handing over a .bot the file does not contain", async () => {
    const store = boundStore("bots/x/main.bot");
    api.unparse.mockResolvedValue({
      source: "THE FILE AS IT IS",
      refused: "the value at line 5 is written over several lines",
    });
    render(
      <DocumentStoreProvider store={store}>
        <Harness />
      </DocumentStoreProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await waitFor(() => {
      const texts = useUIStore.getState().toasts.map((t) => t.message).join("\n");
      expect(texts).toMatch(/cannot be copied/i);
    });
  });

  // A bot in SEVERAL files downloads as its program — one text, which is
  // what a `.bot` on the author's disk means. Asking for it per file would
  // hand over the main alone: an import line and no nodes.
  it("downloads a bot in several files as its program, never as its main alone", async () => {
    const store = boundStore("botsource://t1/demo/main.bot");
    store.getState().setUnit({
      root: "",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    api.unparse.mockResolvedValue({ source: "THE MERGED PROGRAM" });
    render(
      <DocumentStoreProvider store={store}>
        <Harness />
      </DocumentStoreProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    await waitFor(() => expect(api.unparse).toHaveBeenCalled());
    // flatten decides WHAT is rendered; the path travels with it so the
    // answer can still name which of the bot's files the merged text no
    // longer carries over its lines.
    expect(api.unparse.mock.calls[0]?.[1]).toEqual({
      flatten: true,
      path: "botsource://t1/demo/main.bot",
    });
  });
});
