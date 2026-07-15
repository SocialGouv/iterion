import { describe, expect, it } from "vitest";

import type { RunFile, TouchedFile } from "@/api/runs";

import { aggregateProducedItems, type RunProducedSource } from "./producedItems";

function file(partial: Partial<RunFile> & { path: string }): RunFile {
  return { status: "A", added: 0, deleted: 0, ...partial };
}

function touched(path: string, ...nodeIds: string[]): TouchedFile {
  return { path, node_ids: nodeIds, writes: 1, last_seq: 1 };
}

// A worktree-isolated run whose git channel answered: git is authoritative.
function worktreeRun(partial: Partial<RunProducedSource> & { runId: string }): RunProducedSource {
  return {
    workDir: `/wt/${partial.runId}`,
    worktree: true,
    filesAvailable: true,
    ...partial,
  };
}

// An in-place run (no isolated worktree): git rows are gated on touched.
function inPlaceRun(partial: Partial<RunProducedSource> & { runId: string }): RunProducedSource {
  return {
    workDir: "/workspace",
    worktree: false,
    filesAvailable: true,
    ...partial,
  };
}

describe("aggregateProducedItems — ordering & channels", () => {
  it("lists artifacts NEWEST first across runs, then change rows by path", () => {
    const items = aggregateProducedItems([
      worktreeRun({
        runId: "run-root",
        files: [file({ path: "src/b.ts" }), file({ path: "src/a.ts" })],
        artifacts: [
          { path: "renders/old.png", size: 5, modified_at: "2026-07-15T00:00:00Z" },
        ],
      }),
      worktreeRun({
        runId: "run-child",
        files: [file({ path: "child.py" })],
        artifacts: [
          { path: "renders/new.mp4", size: 10, modified_at: "2026-07-15T09:30:00Z" },
        ],
      }),
    ]);
    expect(items.map((i) => `${i.source}:${i.path}`)).toEqual([
      "artifact:renders/new.mp4",
      "artifact:renders/old.png",
      "change:child.py",
      "change:src/a.ts",
      "change:src/b.ts",
    ]);
  });

  it("artifact ties on equal timestamps fall back to path order", () => {
    const items = aggregateProducedItems([
      worktreeRun({
        runId: "run-1",
        artifacts: [
          { path: "b.png", size: 1, modified_at: "2026-07-15T00:00:00Z" },
          { path: "a.png", size: 1, modified_at: "2026-07-15T00:00:00Z" },
        ],
      }),
    ]);
    expect(items.map((i) => i.path)).toEqual(["a.png", "b.png"]);
  });

  it("classifies each item and carries its channel metadata", () => {
    const [artifact, change] = aggregateProducedItems([
      worktreeRun({
        runId: "run-1",
        files: [
          file({ path: "main.go", status: "M", added: 12, deleted: 3, lifecycle: "committed" }),
        ],
        artifacts: [{ path: "song.wav", size: 2048, modified_at: "2026-07-15T00:00:00Z" }],
      }),
    ]);
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
    const items = aggregateProducedItems([
      worktreeRun({
        runId: "run-1",
        files: [file({ path: "gone.ts", status: "D" }), file({ path: "kept.ts" })],
      }),
    ]);
    expect(items.map((i) => i.path)).toEqual(["kept.ts"]);
  });

  it("tolerates still-loading / errored run slots (undefined channels)", () => {
    const items = aggregateProducedItems([
      worktreeRun({ runId: "run-root", files: [file({ path: "root.go" })] }),
      { runId: "run-child" },
    ]);
    expect(items.map((i) => i.path)).toEqual(["root.go"]);
    expect(aggregateProducedItems([])).toEqual([]);
  });
});

describe("aggregateProducedItems — in-place trust gate", () => {
  it("drops git rows no node wrote (ambient operator state) for in-place runs", () => {
    const items = aggregateProducedItems([
      inPlaceRun({
        runId: "run-1",
        files: [
          // The operator's own pre-existing dirty file — must NOT show.
          file({ path: "operator-wip.ts", status: "M", added: 9, deleted: 9 }),
          // Written by a node — kept, with real git counts.
          file({ path: "src/feature.ts", status: "M", added: 4, deleted: 1, lifecycle: "uncommitted" }),
        ],
        touched: [touched("src/feature.ts", "implement")],
      }),
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      path: "src/feature.ts",
      status: "M",
      added: 4,
      deleted: 1,
      nodes: ["implement"],
    });
  });

  it("keeps touched-only rows (e.g. already committed in place) without git counts", () => {
    const items = aggregateProducedItems([
      inPlaceRun({
        runId: "run-1",
        files: [], // committed → no longer in git status
        touched: [touched("docs/spec.md", "write_docs")],
      }),
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      path: "docs/spec.md",
      runId: "run-1",
      source: "change",
      nodes: ["write_docs"],
    });
    expect(items[0]?.status).toBeUndefined();
    expect(items[0]?.added).toBeUndefined();
  });

  it("shows nothing for an in-place run with no touched data (never the raw git status)", () => {
    const items = aggregateProducedItems([
      inPlaceRun({
        runId: "run-1",
        files: [file({ path: "operator-wip.ts", status: "M" })],
      }),
    ]);
    expect(items).toEqual([]);
  });

  it("keeps all git rows for worktree runs, annotated with nodes where known", () => {
    const items = aggregateProducedItems([
      worktreeRun({
        runId: "run-1",
        files: [
          file({ path: "by-node.ts", lifecycle: "uncommitted" }),
          file({ path: "by-bash.sh", lifecycle: "uncommitted" }), // written via Bash — no touched entry
        ],
        touched: [touched("by-node.ts", "implement", "fix")],
      }),
    ]);
    expect(items.map((i) => i.path)).toEqual(["by-bash.sh", "by-node.ts"]);
    expect(items[0]?.nodes).toBeUndefined();
    expect(items[1]?.nodes).toEqual(["implement", "fix"]);
  });

  it("falls back to touched rows when a worktree run's git channel is unavailable", () => {
    // Cloud run mid-flight: worktree=true but the server pod has neither
    // the worktree nor a gitmeta snapshot (available=false, "building").
    const items = aggregateProducedItems([
      worktreeRun({
        runId: "run-1",
        files: undefined,
        filesAvailable: false,
        touched: [touched("src/feature.ts", "implement")],
      }),
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      path: "src/feature.ts",
      nodes: ["implement"],
    });
    expect(items[0]?.status).toBeUndefined();
  });

  it("suppresses touched rows for paths git reports deleted", () => {
    const items = aggregateProducedItems([
      inPlaceRun({
        runId: "run-1",
        files: [file({ path: "scratch-notes.md", status: "D" })],
        touched: [touched("scratch-notes.md", "write_notes"), touched("kept.md", "write_notes")],
      }),
    ]);
    expect(items.map((i) => i.path)).toEqual(["kept.md"]);
  });
});

describe("aggregateProducedItems — shared-workdir dedupe", () => {
  it("dedupes a path across runs sharing one working directory", () => {
    const shared = { workDir: "/wt/pipeline", worktree: true, filesAvailable: true };
    const items = aggregateProducedItems([
      { runId: "run-parent", ...shared, files: [file({ path: "shared.txt", lifecycle: "uncommitted" })] },
      {
        runId: "run-child",
        ...shared,
        files: [file({ path: "shared.txt", lifecycle: "uncommitted" })],
        touched: [touched("shared.txt", "child_node")],
      },
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]?.nodes).toEqual(["child_node"]);
  });

  it("a committed row keeps the run whose branch range recorded it", () => {
    const shared = { workDir: "/wt/pipeline", worktree: true, filesAvailable: true };
    const items = aggregateProducedItems([
      { runId: "run-parent", ...shared, files: [] },
      {
        runId: "run-child",
        ...shared,
        files: [file({ path: "committed.go", status: "M", lifecycle: "committed" })],
      },
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ runId: "run-child", lifecycle: "committed" });
  });

  it("prefers the uncommitted entry on a lifecycle collision across runs", () => {
    const shared = { workDir: "/wt/pipeline", worktree: true, filesAvailable: true };
    const items = aggregateProducedItems([
      {
        runId: "run-parent",
        ...shared,
        files: [file({ path: "hot.ts", status: "M", added: 1, deleted: 0, lifecycle: "committed" })],
      },
      {
        runId: "run-child",
        ...shared,
        files: [file({ path: "hot.ts", status: "M", added: 7, deleted: 2, lifecycle: "uncommitted" })],
      },
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      runId: "run-child",
      lifecycle: "uncommitted",
      added: 7,
      deleted: 2,
    });
  });

  it("keeps distinct working directories separate (distinct keys, both rows)", () => {
    const items = aggregateProducedItems([
      worktreeRun({ runId: "run-a", files: [file({ path: "same.txt" })] }),
      worktreeRun({ runId: "run-b", files: [file({ path: "same.txt" })] }),
    ]);
    expect(items).toHaveLength(2);
    expect(new Set(items.map((i) => i.key)).size).toBe(2);
  });

  it("groups by run id while workDir is unknown (files query still loading)", () => {
    const items = aggregateProducedItems([
      { runId: "run-a", worktree: true, filesAvailable: true, files: [file({ path: "x.txt" })] },
      { runId: "run-b", worktree: true, filesAvailable: true, files: [file({ path: "x.txt" })] },
    ]);
    expect(items).toHaveLength(2);
  });
});
