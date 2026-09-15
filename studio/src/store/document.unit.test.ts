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
