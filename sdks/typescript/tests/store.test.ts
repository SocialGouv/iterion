import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  listRuns,
  loadArtifact,
  loadInteraction,
  loadRun,
} from "../src/index.js";
import type { Artifact, Interaction, Run } from "../src/index.js";
import { makeTmpStore, type TmpStore } from "./helpers/tmpStore.js";

describe("store helpers", () => {
  let store: TmpStore;

  beforeEach(async () => {
    store = await makeTmpStore("iterion-sdk-store-");
    await store.ensureRunDir("run_1");

    const run: Run = {
      format_version: 1,
      id: "run_1",
      workflow_name: "demo",
      status: "finished",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:01:00Z",
      artifact_index: { analyze: 1 },
    };
    await store.writeFile("run_1", "run.json", run);

    const a0: Artifact = {
      run_id: "run_1",
      node_id: "analyze",
      version: 0,
      data: { summary: "first" },
      written_at: "2026-01-01T00:00:30Z",
    };
    const a1: Artifact = { ...a0, version: 1, data: { summary: "second" } };
    await store.writeFile("run_1", "artifacts/analyze/0.json", a0);
    await store.writeFile("run_1", "artifacts/analyze/1.json", a1);

    const interaction: Interaction = {
      id: "run_1_review",
      run_id: "run_1",
      node_id: "review",
      requested_at: "2026-01-01T00:01:00Z",
      questions: { summary: "ok?" },
    };
    await store.writeFile("run_1", "interactions/run_1_review.json", interaction);
  });

  afterEach(async () => {
    await store.cleanup();
  });

  it("loadRun reads run.json", async () => {
    const run = await loadRun("run_1", { storeDir: store.storeDir });
    expect(run.workflow_name).toBe("demo");
    expect(run.status).toBe("finished");
  });

  it("loadInteraction reads interactions/<id>.json", async () => {
    const inter = await loadInteraction("run_1", "run_1_review", { storeDir: store.storeDir });
    expect(inter.questions).toEqual({ summary: "ok?" });
  });

  it("loadArtifact returns the requested version", async () => {
    const a = await loadArtifact("run_1", "analyze", 0, { storeDir: store.storeDir });
    expect(a.data).toEqual({ summary: "first" });
  });

  it("loadArtifact returns the latest via artifact_index when version omitted", async () => {
    const a = await loadArtifact("run_1", "analyze", undefined, { storeDir: store.storeDir });
    expect(a.version).toBe(1);
    expect(a.data).toEqual({ summary: "second" });
  });

  it("loadArtifact falls back to directory scan when index missing", async () => {
    const minimal: Run = {
      format_version: 1,
      id: "run_1",
      workflow_name: "demo",
      status: "finished",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:01:00Z",
    };
    await store.writeFile("run_1", "run.json", minimal);
    const a = await loadArtifact("run_1", "analyze", undefined, { storeDir: store.storeDir });
    expect(a.version).toBe(1);
  });

  it("rejects traversal in public store helper path components", async () => {
    const escapedRun = { id: "../../secret", workflow_name: "escaped" };
    await mkdir(join(store.tmp, "secret"), { recursive: true });
    await writeFile(join(store.tmp, "secret", "run.json"), JSON.stringify(escapedRun));

    await expect(loadRun("../../secret", { storeDir: store.storeDir })).rejects.toThrow(
      /path traversal|path separator|safe path component/,
    );
    await expect(loadRun("..\\..\\secret", { storeDir: store.storeDir })).rejects.toThrow(
      /path traversal|path separator|safe path component/,
    );
    await expect(loadInteraction("run_1", "../../secret", { storeDir: store.storeDir })).rejects.toThrow(
      /path traversal|path separator|safe path component/,
    );
    await expect(loadInteraction("run_1", "..\\..\\secret", { storeDir: store.storeDir })).rejects.toThrow(
      /path traversal|path separator|safe path component/,
    );
    await expect(loadArtifact("run_1", "../../secret", 0, { storeDir: store.storeDir })).rejects.toThrow(
      /path traversal|path separator|safe path component/,
    );
    await expect(loadArtifact("run_1", "..\\..\\secret", 0, { storeDir: store.storeDir })).rejects.toThrow(
      /path traversal|path separator|safe path component/,
    );
  });

  it("listRuns returns the directory entries", async () => {
    expect(await listRuns({ storeDir: store.storeDir })).toEqual(["run_1"]);
  });

  it("listRuns returns [] when the store has no runs", async () => {
    const empty = await makeTmpStore("iterion-sdk-store-empty-");
    try {
      expect(await listRuns({ storeDir: empty.storeDir })).toEqual([]);
    } finally {
      await empty.cleanup();
    }
  });
});
