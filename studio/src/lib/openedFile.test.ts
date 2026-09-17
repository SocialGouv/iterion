import { describe, expect, it } from "vitest";

import type { IterDocument, UnitInfo } from "@/api/types";
import { createDocumentStore } from "@/store/document";

import { applyOpenedFile } from "./openedFile";
import { salvageRefusal } from "./salvage";

const document = { workflows: [] } as unknown as IterDocument;

// The real store, not a double: its setCurrentFilePath clears the unit and
// the salvage flag, which is the coupling that decides what survives a bind.
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
    expect(store.getState().salvaged).toBe(false);
  });

  // The defect: the document is what the parser SALVAGED, so writing it back
  // puts the parser's reading over what the author wrote.
  it("marks a file that does not parse a salvage, and keeps its path", () => {
    const source = "workflow y:\n  entry: done\n\nagent broken\n  not a declaration\n";
    const store = createDocumentStore();

    applyOpenedFile(
      {
        source,
        document,
        diagnostics: ["y.bot:4:1: error [E012]: unknown property"],
        path: "bots/y/main.bot",
        bindable: false,
      },
      store.getState(),
    );

    // The path stays: this editor is ABOUT that file, and the watcher, the
    // tab binding and the validation scope all read it. What the flag
    // refuses is the write.
    expect(store.getState().currentFilePath).toBe("bots/y/main.bot");
    expect(store.getState().salvaged).toBe(true);
    expect(salvageRefusal(store.getState())).not.toBeNull();
    // The text is still shown: the editor has to display what is there.
    expect(store.getState().currentSource).toBe(source);
    expect(store.getState().diagnostics).toHaveLength(1);
  });

  // The write that repairs the file arrives through the watcher, which
  // applies it here: the flag has to LIFT, or the file stays unwritable for
  // the rest of the session.
  it("lifts the flag when the file parses again", () => {
    const store = createDocumentStore();
    applyOpenedFile(
      { source: "broken\n", document, diagnostics: ["e"], path: "mine.bot", bindable: false },
      store.getState(),
    );
    expect(store.getState().salvaged).toBe(true);

    applyOpenedFile(
      { source: "workflow ok:\n  entry: done\n", document, diagnostics: [], path: "mine.bot" },
      store.getState(),
    );
    expect(store.getState().salvaged).toBe(false);
    expect(salvageRefusal(store.getState())).toBeNull();
  });

  // An answer a client builds for a program it holds itself carries no
  // verdict. Absent must read as writable, or opening one of those would
  // hand the author a buffer they cannot save.
  it("treats an absent verdict as writable", () => {
    const store = createDocumentStore();
    applyOpenedFile(
      { source: "workflow a:\n  entry: done\n", document, diagnostics: [], path: "a.bot" },
      store.getState(),
    );
    expect(store.getState().salvaged).toBe(false);
  });
});
