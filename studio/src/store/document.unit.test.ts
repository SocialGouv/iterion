import { describe, expect, it } from "vitest";
import { createDocumentStore } from "./document";
import { createEmptyDocument } from "@/lib/defaults";

describe("document store: the unit a document was opened from", () => {
  it("keeps the unit across document edits and drops it when the current file changes", () => {
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("demo/main.bot");
    store.getState().setUnit({
      root: "demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot", imports: ["lib/nodes.bot"] }, { rel: "lib/nodes.bot" }],
    });
    expect(store.getState().unit?.revision).toBe("r1");

    // An edit is not a change of file: the revision the save must present stays.
    store.getState().setDocument(createEmptyDocument());
    expect(store.getState().unit?.revision).toBe("r1");

    // A new file, a Save As, an import: a unit belongs to a file.
    store.getState().setCurrentFilePath(null);
    expect(store.getState().unit).toBeNull();
  });
});

describe("document store: a null path that was SET, told from one never set", () => {
  it("is not detached when fresh, is once the path is set to null, and is not again once a path is bound", () => {
    const store = createDocumentStore();
    // A fresh store: bound to nothing, and not resolved yet — a tab keeps
    // the file param its load is for.
    expect(store.getState().currentFilePath).toBeNull();
    expect(store.getState().detached).toBe(false);

    // File → New, Import, Start blank: the document follows no file.
    store.getState().setCurrentFilePath(null);
    expect(store.getState().detached).toBe(true);

    // Save As, an opened file: bound again.
    store.getState().setCurrentFilePath("demo/main.bot");
    expect(store.getState().detached).toBe(false);
  });
});
