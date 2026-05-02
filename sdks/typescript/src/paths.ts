/**
 * Shared path resolution for the run store layout documented in
 * `docs/persisted-formats.md`.
 */

import { readFile } from "node:fs/promises";
import { basename, join, normalize } from "node:path";

export const DEFAULT_STORE_DIR = ".iterion";

export function resolveStoreDir(storeDir: string | undefined): string {
  return storeDir ?? DEFAULT_STORE_DIR;
}

/**
 * Validate a public run-store path component before joining it into the
 * on-disk store layout. Mirrors the Go store's defensive checks so run IDs,
 * node IDs, and interaction IDs supplied from users/URLs cannot escape the
 * configured store directory.
 */
export function sanitizePathComponent(name: string, component: string): string {
  if (component === "") {
    throw new Error(`store: ${name} must not be empty`);
  }
  if (component === "." || component === ".." || component.includes("..")) {
    throw new Error(`store: ${name} ${JSON.stringify(component)} contains path traversal`);
  }
  if (component.includes("/") || component.includes("\\")) {
    throw new Error(`store: ${name} ${JSON.stringify(component)} contains path separator`);
  }
  if (/[\x00-\x1F\x7F]/u.test(component)) {
    throw new Error(`store: ${name} ${JSON.stringify(component)} contains control character`);
  }
  if (basename(component) !== component || normalize(component) !== component) {
    throw new Error(`store: ${name} ${JSON.stringify(component)} is not a safe path component`);
  }
  return component;
}

export function runDir(runId: string, storeDir: string | undefined): string {
  return join(resolveStoreDir(storeDir), "runs", sanitizePathComponent("run ID", runId));
}

export function eventsPath(runId: string, storeDir: string | undefined): string {
  return join(runDir(runId, storeDir), "events.jsonl");
}

export function runJsonPath(runId: string, storeDir: string | undefined): string {
  return join(runDir(runId, storeDir), "run.json");
}

export function interactionPath(
  runId: string,
  interactionId: string,
  storeDir: string | undefined,
): string {
  return join(
    runDir(runId, storeDir),
    "interactions",
    `${sanitizePathComponent("interaction ID", interactionId)}.json`,
  );
}

export function artifactDir(
  runId: string,
  nodeId: string,
  storeDir: string | undefined,
): string {
  return join(
    runDir(runId, storeDir),
    "artifacts",
    sanitizePathComponent("node ID", nodeId),
  );
}

export async function readJSON<T>(path: string): Promise<T> {
  const raw = await readFile(path, "utf8");
  return JSON.parse(raw) as T;
}
