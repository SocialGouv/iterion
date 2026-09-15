import { beforeEach, describe, expect, it, vi } from "vitest";

import type { IterDocument, UnitInfo } from "@/api/types";

import { openExampleIntoStore, type ExampleTargetStore } from "./openExample";

const loadExample = vi.fn();
vi.mock("@/api/client", () => ({
  loadExample: (...args: unknown[]) => loadExample(...args),
}));

function targetStore(): ExampleTargetStore & { unit: UnitInfo | null | undefined } {
  const store = {
    unit: undefined as UnitInfo | null | undefined,
    setDocument: vi.fn(),
    setDiagnostics: vi.fn(),
    setCurrentSource: vi.fn(),
    setCurrentFilePath: vi.fn(),
    setUnit: vi.fn((unit: UnitInfo | null) => {
      store.unit = unit;
    }),
    markSaved: vi.fn(),
  };
  return store;
}

const document = { workflows: [] } as unknown as IterDocument;

describe("openExampleIntoStore", () => {
  beforeEach(() => {
    loadExample.mockReset();
  });

  it("binds the unit of an example that is a bot in several files on disk", async () => {
    const unit: UnitInfo = {
      root: "bots/x",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    };
    loadExample.mockResolvedValue({ source: "import \"lib/nodes.bot\"\n", document, diagnostics: [], unit });
    const store = targetStore();
    await openExampleIntoStore("x/main.bot", store);
    expect(store.setUnit).toHaveBeenCalledWith(unit);
    expect(store.unit).toBe(unit);
    expect(store.setCurrentFilePath).toHaveBeenCalledWith("bots/x/main.bot");
  });

  it("clears the unit for an example served as one program", async () => {
    loadExample.mockResolvedValue({ source: "workflow x:\n  entry: done\n", document, diagnostics: [] });
    const store = targetStore();
    store.unit = { root: "bots/old", main: "main.bot", revision: "r0", files: [] };
    await openExampleIntoStore("feature-dev/main.bot", store);
    expect(store.setUnit).toHaveBeenCalledWith(null);
    expect(store.unit).toBeNull();
  });
});
