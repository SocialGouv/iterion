/**
 * Shared scaffolding for tests that need a temporary run-store directory.
 *
 * Mirrors the Go `tmpStore()` helper used by iterion's e2e tests
 * (`CLAUDE.md` § Testing Patterns).
 */

import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { runDir as resolveRunDir } from "../../src/paths.js";

export interface TmpStore {
  /** Root tmp directory holding the store. */
  tmp: string;
  /** Resolved store directory (`<tmp>/store`). */
  storeDir: string;
  /** Compute `<storeDir>/runs/<runId>` and ensure it exists. */
  ensureRunDir(runId: string): Promise<string>;
  /** Write JSON content under `<runDir>/<relativePath>`, creating directories as needed. */
  writeFile(runId: string, relativePath: string, content: string | object): Promise<string>;
  /** Remove the temp directory. Safe to call twice. */
  cleanup(): Promise<void>;
}

export async function makeTmpStore(prefix = "iterion-sdk-"): Promise<TmpStore> {
  const tmp = await mkdtemp(join(tmpdir(), prefix));
  const storeDir = join(tmp, "store");
  let removed = false;

  return {
    tmp,
    storeDir,
    async ensureRunDir(runId: string): Promise<string> {
      const dir = resolveRunDir(runId, storeDir);
      await mkdir(dir, { recursive: true });
      return dir;
    },
    async writeFile(runId, relativePath, content) {
      const target = join(resolveRunDir(runId, storeDir), relativePath);
      const lastSep = target.lastIndexOf("/");
      if (lastSep > 0) {
        await mkdir(target.slice(0, lastSep), { recursive: true });
      }
      const body = typeof content === "string" ? content : JSON.stringify(content);
      await writeFile(target, body);
      return target;
    },
    async cleanup() {
      if (removed) return;
      removed = true;
      await rm(tmp, { recursive: true, force: true });
    },
  };
}
