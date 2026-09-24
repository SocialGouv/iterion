import { beforeEach, describe, expect, it, vi } from "vitest";

import type { IterDocument, UnitInfo } from "@/api/types";
import { createDocumentStore } from "@/store/document";

import { openExampleIntoStore } from "./openExample";

const loadExample = vi.fn();
vi.mock("@/api/client", () => ({
  loadExample: (...args: unknown[]) => loadExample(...args),
}));

const document = { workflows: [] } as unknown as IterDocument;

// The real store, not a double: its setCurrentFilePath clears the unit, the
// coupling that decides whether the unit an example binds survives.
describe("openExampleIntoStore", () => {
  beforeEach(() => {
    loadExample.mockReset();
  });

  it("binds the unit and the path the server named, for a bot in several files inside the workspace", async () => {
    const unit: UnitInfo = {
      root: "examples/x",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot", imports: ["lib/nodes.bot"] }, { rel: "lib/nodes.bot" }],
    };
    loadExample.mockResolvedValue({
      source: 'import "lib/nodes.bot"\n',
      document,
      diagnostics: [],
      path: "examples/x/main.bot",
      unit,
    });
    const store = createDocumentStore();
    await openExampleIntoStore("x/main.bot", store);
    expect(store.getState().currentFilePath).toBe("examples/x/main.bot");
    expect(store.getState().unit).toEqual(unit);
  });

  it("keeps the path of an example that does not parse, and marks it a salvage", async () => {
    loadExample.mockResolvedValue({
      source: "workflow y:\n  entry: done\n!!! broken @@@\n",
      document,
      diagnostics: ["y/main.bot:3:1: error [E001]: unexpected character"],
      path: "catalog/y/main.bot",
      bindable: false,
    });
    const store = createDocumentStore();
    await openExampleIntoStore("y/main.bot", store);
    // The editor is about that file: the watcher, the tab binding and the
    // validation scope all read the path. What is refused is the write.
    expect(store.getState().currentFilePath).toBe("catalog/y/main.bot");
    expect(store.getState().salvaged).toBe(true);
    expect(store.getState().unit).toBeNull();
    expect(store.getState().currentSource).toBe("workflow y:\n  entry: done\n!!! broken @@@\n");
  });

  // A program the workspace does not hold — an embedded bot, a catalog
  // outside it — is named bots/<name>, where a save of it would LAND. It is
  // still a salvage, so no save reaches that file either.
  it("falls back to bots/<name> for an unparseable program with no workspace path", async () => {
    loadExample.mockResolvedValue({
      source: "workflow z:\n  entry: done\n!!! broken @@@\n",
      document,
      diagnostics: ["z/main.bot:3:1: error [E001]: unexpected character"],
      bindable: false,
    });
    const store = createDocumentStore();
    await openExampleIntoStore("z/main.bot", store);
    expect(store.getState().currentFilePath).toBe("bots/z/main.bot");
    expect(store.getState().salvaged).toBe(true);
  });

  it("binds bots/<name> and no unit for an example served as one program", async () => {
    loadExample.mockResolvedValue({ source: "workflow x:\n  entry: done\n", document, diagnostics: [] });
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("bots/old/main.bot");
    store.getState().setUnit({ root: "bots/old", main: "main.bot", revision: "r0", files: [] });
    await openExampleIntoStore("feature-dev/main.bot", store);
    expect(store.getState().currentFilePath).toBe("bots/feature-dev/main.bot");
    expect(store.getState().salvaged).toBe(false);
    expect(store.getState().unit).toBeNull();
  });
});

describe("openExampleIntoStore, answering after the author worked", () => {
  it("does not land over edits made while the example loaded, and says why", async () => {
    let land!: (v: unknown) => void;
    loadExample.mockReturnValue(new Promise((r) => (land = r)));
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("bots/mine.bot");
    store.getState().markSaved();
    const opening = openExampleIntoStore("x/main.bot", store);
    const edited = { workflows: [], comments: [{ text: "typed meanwhile" }] } as unknown as IterDocument;
    store.getState().setDocument(edited);

    land({ source: "x\n", document, diagnostics: [], path: "examples/x/main.bot" });
    expect(await opening).toBe("refused");
    expect(store.getState().currentFilePath).toBe("bots/mine.bot");
    expect(store.getState().document?.comments?.[0]?.text).toBe("typed meanwhile");
  });
});
