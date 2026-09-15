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

  it("binds bots/<name> and no unit for an example served as one program", async () => {
    loadExample.mockResolvedValue({ source: "workflow x:\n  entry: done\n", document, diagnostics: [] });
    const store = createDocumentStore();
    store.getState().setCurrentFilePath("bots/old/main.bot");
    store.getState().setUnit({ root: "bots/old", main: "main.bot", revision: "r0", files: [] });
    await openExampleIntoStore("feature-dev/main.bot", store.getState());
    expect(store.getState().currentFilePath).toBe("bots/feature-dev/main.bot");
    expect(store.getState().unit).toBeNull();
  });
});
