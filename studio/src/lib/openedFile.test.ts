import { describe, expect, it } from "vitest";

import type { IterDocument, UnitInfo } from "@/api/types";
import { createDocumentStore } from "@/store/document";

import { applyOpenedFile } from "./openedFile";

const document = { workflows: [] } as unknown as IterDocument;

// The real store, not a double: its setCurrentFilePath clears the unit, which
// is the coupling that decides whether a unit survives being bound.
describe("applyOpenedFile", () => {
  it("binds the path and the unit the server named", () => {
    const unit: UnitInfo = {
      root: "bots/x",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot", imports: ["lib/nodes.bot"] }, { rel: "lib/nodes.bot" }],
    };
    const store = createDocumentStore();
    applyOpenedFile(
      { source: 'import "lib/nodes.bot"\n', document, diagnostics: [], path: "bots/x/main.bot", unit },
      store.getState(),
    );
    expect(store.getState().currentFilePath).toBe("bots/x/main.bot");
    expect(store.getState().unit).toEqual(unit);
  });

  // The defect: the document is what the parser SALVAGED, so binding it makes
  // the first Save write that back over what the author wrote.
  it("binds nothing when the server named no path — a save must ask where", () => {
    const source = "workflow y:\n  entry: done\n\nagent broken\n  not a declaration\n";
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("bots/y/main.bot");

    applyOpenedFile(
      { source, document, diagnostics: ["y.bot:4:1: error [E012]: unknown property"] },
      store.getState(),
    );

    expect(store.getState().currentFilePath).toBeNull();
    expect(store.getState().unit).toBeNull();
    // The text is still shown: the editor has to display what is there.
    expect(store.getState().currentSource).toBe(source);
    expect(store.getState().diagnostics).toHaveLength(1);
  });

  // Unbinding without this stops the tab following the file for good: every
  // later file_modified bails on the missing path, so the write that REPAIRS
  // the file never reaches it, and the canvas keeps a salvage marked saved.
  it("keeps following the file it opened, so a later write can rebind it", () => {
    const store = createDocumentStore();

    applyOpenedFile({ source: "broken\n", document, diagnostics: ["e"] }, store.getState(), "mine.bot");
    expect(store.getState().currentFilePath).toBeNull();
    expect(store.getState().watchedFilePath).toBe("mine.bot");

    // The next write makes it parse: the tab binds it again.
    applyOpenedFile(
      { source: "workflow ok:\n  entry: done\n", document, diagnostics: [], path: "mine.bot" },
      store.getState(),
      "mine.bot",
    );
    expect(store.getState().currentFilePath).toBe("mine.bot");
    expect(store.getState().watchedFilePath).toBe("mine.bot");
  });

  // A tab counts as HYDRATED from the file it follows. Keyed on the binding,
  // a file that does not parse never counts, so every remount — leaving the
  // editor and coming back, a project switch, StrictMode's double mount —
  // re-fetches and replaces the author's in-progress repair with the salvage
  // from disk. That is the loss #1251 is about, on exactly its files.
  it("counts as hydrated from the file it follows, bound or not", () => {
    const store = createDocumentStore();
    applyOpenedFile({ source: "broken\n", document, diagnostics: ["e"] }, store.getState(), "mine.bot");

    const s = store.getState();
    expect(s.currentFilePath ?? s.watchedFilePath).toBe("mine.bot");
  });

  // File→New, Import and Start-blank unbind and stop there. A tab that kept
  // following the previous file would auto-reload it over the new document,
  // with no user action — so the followed path tracks the binding, null
  // included, and only applyOpenedFile parts them.
  it("stops following when the tab is unbound by anything but a failed parse", () => {
    const store = createDocumentStore();
    applyOpenedFile(
      { source: "workflow a:\n  entry: done\n", document, diagnostics: [], path: "a.bot" },
      store.getState(),
      "a.bot",
    );
    expect(store.getState().watchedFilePath).toBe("a.bot");

    store.getState().setCurrentFilePath(null); // File → New
    expect(store.getState().watchedFilePath).toBeNull();
  });
});
