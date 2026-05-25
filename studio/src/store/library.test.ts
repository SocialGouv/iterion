import { afterEach, describe, expect, it, vi } from "vitest";

function stubStorage(opts: {
  stored?: string | null;
  setItem?: (key: string, value: string) => void;
}) {
  vi.stubGlobal("window", {
    localStorage: {
      getItem: () => opts.stored ?? null,
      setItem: opts.setItem ?? (() => {}),
    },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
});

describe("library store persistence", () => {
  it("ignores malformed stored payloads instead of crashing module init", async () => {
    stubStorage({ stored: "{}" });

    const { useLibraryStore } = await import("./library");

    expect(useLibraryStore.getState().customItems).toEqual([]);
  });

  it("keeps in-memory edits when localStorage writes fail", async () => {
    stubStorage({
      stored: "[]",
      setItem: () => {
        throw new Error("quota exceeded");
      },
    });
    const { useLibraryStore } = await import("./library");

    expect(() =>
      useLibraryStore.getState().addCustomItem({
        id: "custom-1",
        name: "Custom",
        description: "Custom node",
        category: "agent",
        builtin: false,
        template: { node: { kind: "agent", data: {} } },
      }),
    ).not.toThrow();

    expect(useLibraryStore.getState().customItems).toHaveLength(1);
  });
});
