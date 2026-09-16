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
    await openExampleIntoStore("x/main.bot", store.getState());
    expect(store.getState().currentFilePath).toBe("examples/x/main.bot");
    expect(store.getState().unit).toEqual(unit);
  });

  it("binds nothing for an example the server says is not bindable — a save must ask where", async () => {
    loadExample.mockResolvedValue({
      source: "workflow y:\n  entry: done\n!!! broken @@@\n",
      document,
      diagnostics: ["y/main.bot:3:1: error [E001]: unexpected character"],
      bindable: false,
      followed_path: "bots/y/main.bot",
    });
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("bots/y/main.bot");
    await openExampleIntoStore("y/main.bot", store.getState());
    expect(store.getState().currentFilePath).toBeNull();
    expect(store.getState().unit).toBeNull();
    expect(store.getState().currentSource).toBe("workflow y:\n  entry: done\n!!! broken @@@\n");
    // Unbound, still followed: the write that repairs the file reaches this
    // tab and rebinds it. Unfollowed, the tab never sees that write, and the
    // next remount applies the file it still names over the repair.
    expect(store.getState().watchedFilePath).toBe("bots/y/main.bot");
  });

  // The server names no file for a program the workspace does not hold — an
  // embedded bot, a catalog outside it. `bots/<name>` is where a save would
  // LAND, not a file to reload from: following it would reload some other
  // file over this buffer, the very loss this rule exists to prevent.
  it("follows nothing when the server names no file for an unbindable program", async () => {
    loadExample.mockResolvedValue({
      source: "workflow z:\n  entry: done\n!!! broken @@@\n",
      document,
      diagnostics: ["z/main.bot:3:1: error [E001]: unexpected character"],
      bindable: false,
    });
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("bots/old/main.bot");
    await openExampleIntoStore("z/main.bot", store.getState());
    expect(store.getState().currentFilePath).toBeNull();
    expect(store.getState().watchedFilePath).toBeNull();
  });

  it("binds bots/<name> and no unit for an example served as one program", async () => {
    loadExample.mockResolvedValue({ source: "workflow x:\n  entry: done\n", document, diagnostics: [] });
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("bots/old/main.bot");
    store.getState().setUnit({ root: "bots/old", main: "main.bot", revision: "r0", files: [] });
    await openExampleIntoStore("feature-dev/main.bot", store.getState());
    expect(store.getState().currentFilePath).toBe("bots/feature-dev/main.bot");
    // A bound tab follows what it bound, never a second path.
    expect(store.getState().watchedFilePath).toBe("bots/feature-dev/main.bot");
    expect(store.getState().unit).toBeNull();
  });
});
