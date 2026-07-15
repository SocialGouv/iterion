import { describe, expect, it } from "vitest";

import type { RunFile } from "@/api/runs";

import { aggregateProducedItems, mergeProducedItems } from "./producedItems";

function file(partial: Partial<RunFile> & { path: string }): RunFile {
  return { status: "A", added: 0, deleted: 0, ...partial };
}

describe("mergeProducedItems", () => {
  it("lists artifacts NEWEST first, then worktree changes by path", () => {
    const items = mergeProducedItems(
      [file({ path: "src/b.ts" }), file({ path: "src/a.ts" })],
      [
        { path: "renders/old.png", size: 5, modified_at: "2026-07-15T00:00:00Z" },
        { path: "renders/new.mp4", size: 10, modified_at: "2026-07-15T09:30:00Z" },
      ],
      "run-1",
    );
    // The freshest output leads — it's the one a pending review is about.
    expect(items.map((i) => i.path)).toEqual([
      "renders/new.mp4",
      "renders/old.png",
      "src/a.ts",
      "src/b.ts",
    ]);
    expect(items.map((i) => i.source)).toEqual([
      "artifact",
      "artifact",
      "change",
      "change",
    ]);
    expect(items.every((i) => i.runId === "run-1")).toBe(true);
  });

  it("ties on equal timestamps fall back to path order", () => {
    const items = mergeProducedItems(
      [],
      [
        { path: "b.png", size: 1, modified_at: "2026-07-15T00:00:00Z" },
        { path: "a.png", size: 1, modified_at: "2026-07-15T00:00:00Z" },
      ],
      "run-1",
    );
    expect(items.map((i) => i.path)).toEqual(["a.png", "b.png"]);
  });

  it("classifies each item and carries its channel metadata", () => {
    const [artifact, change] = mergeProducedItems(
      [file({ path: "main.go", status: "M", added: 12, deleted: 3, lifecycle: "committed" })],
      [{ path: "song.wav", size: 2048, modified_at: "2026-07-15T00:00:00Z" }],
      "run-1",
    );
    expect(artifact).toMatchObject({
      source: "artifact",
      kind: "audio",
      name: "song.wav",
      size: 2048,
      runId: "run-1",
    });
    expect(change).toMatchObject({
      source: "change",
      kind: "code",
      name: "main.go",
      status: "M",
      added: 12,
      deleted: 3,
      lifecycle: "committed",
      runId: "run-1",
    });
  });

  it("drops pure deletions — a removed file is not produced", () => {
    const items = mergeProducedItems(
      [file({ path: "gone.ts", status: "D" }), file({ path: "kept.ts", status: "A" })],
      [],
      "run-1",
    );
    expect(items.map((i) => i.path)).toEqual(["kept.ts"]);
  });

  it("handles missing channels", () => {
    expect(mergeProducedItems(undefined, undefined, "run-1")).toEqual([]);
    expect(mergeProducedItems([], [], "run-1")).toEqual([]);
  });

  it("scopes keys by run so the same path across runs stays distinct", () => {
    const keys = [
      ...mergeProducedItems([file({ path: "shared.txt" })], [], "run-a"),
      ...mergeProducedItems([file({ path: "shared.txt" })], [], "run-b"),
    ].map((i) => i.key);
    expect(new Set(keys).size).toBe(keys.length);
    expect(keys).toEqual(["change:run-a:shared.txt", "change:run-b:shared.txt"]);
  });
});

describe("aggregateProducedItems", () => {
  it("merges every run in the tree, freshest artifact first across runs", () => {
    const items = aggregateProducedItems(
      ["run-root", "run-child"],
      [[file({ path: "root.go" })], [file({ path: "child.py" })]],
      [
        // The root's artifact is NEWER than the child's — it must lead even
        // though the root comes first in the tree and "r" sorts after "c".
        [{ path: "root.png", size: 1, modified_at: "2026-07-15T10:00:00Z" }],
        [{ path: "child.mp3", size: 2, modified_at: "2026-07-15T08:00:00Z" }],
      ],
    );
    expect(items.map((i) => `${i.source}:${i.runId}:${i.path}`)).toEqual([
      "artifact:run-root:root.png",
      "artifact:run-child:child.mp3",
      "change:run-child:child.py",
      "change:run-root:root.go",
    ]);
  });

  it("tolerates still-loading / errored run slots (undefined data)", () => {
    const items = aggregateProducedItems(
      ["run-root", "run-child"],
      [[file({ path: "root.go" })], undefined],
      [undefined, [{ path: "child.mp3", size: 2, modified_at: "2026-07-15T00:00:00Z" }]],
    );
    expect(items.map((i) => i.path)).toEqual(["child.mp3", "root.go"]);
  });
});
