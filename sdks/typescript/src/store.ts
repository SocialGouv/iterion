/**
 * Direct readers for the iterion run store.
 *
 * The store is a documented public format (`docs/persisted-formats.md`),
 * so reading it without going through the CLI is supported and is
 * notably faster for repeated lookups (events tail, artifact diffing).
 */

import { readdir } from "node:fs/promises";
import { join } from "node:path";

import {
  artifactDir,
  interactionPath,
  readJSON,
  resolveStoreDir,
  runJsonPath,
} from "./paths.js";
import type { Artifact, Interaction, Run } from "./types.js";

/** Read `<store>/runs/<runId>/run.json`. */
export async function loadRun(
  runId: string,
  opts: { storeDir?: string } = {},
): Promise<Run> {
  return readJSON<Run>(runJsonPath(runId, opts.storeDir));
}

/** Read `<store>/runs/<runId>/interactions/<interactionId>.json`. */
export async function loadInteraction(
  runId: string,
  interactionId: string,
  opts: { storeDir?: string } = {},
): Promise<Interaction> {
  return readJSON<Interaction>(interactionPath(runId, interactionId, opts.storeDir));
}

/**
 * Read an artifact for a given node. When `version` is omitted, the
 * latest version (per `run.json`'s `artifact_index`, falling back to a
 * directory scan) is returned.
 */
export async function loadArtifact(
  runId: string,
  nodeId: string,
  version?: number,
  opts: { storeDir?: string } = {},
): Promise<Artifact> {
  const dir = artifactDir(runId, nodeId, opts.storeDir);
  let v = version;
  if (v === undefined) {
    try {
      const run = await loadRun(runId, opts);
      const idx = run.artifact_index?.[nodeId];
      if (typeof idx === "number") {
        v = idx;
      }
    } catch {
      // run.json absent or unreadable → fall through to directory scan.
    }
  }
  if (v === undefined) {
    const entries = await readdir(dir);
    const versions = entries
      .map((e) => Number(e.replace(/\.json$/, "")))
      .filter((n) => Number.isFinite(n));
    if (versions.length === 0) {
      throw new Error(`no artifacts found for node ${nodeId} in run ${runId}`);
    }
    v = Math.max(...versions);
  }
  return readJSON<Artifact>(join(dir, `${v}.json`));
}

/** List run IDs present in the store. */
export async function listRuns(
  opts: { storeDir?: string } = {},
): Promise<string[]> {
  const dir = join(resolveStoreDir(opts.storeDir), "runs");
  try {
    const entries = await readdir(dir, { withFileTypes: true });
    return entries.filter((e) => e.isDirectory()).map((e) => e.name);
  } catch {
    return [];
  }
}
