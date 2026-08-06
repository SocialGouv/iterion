import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));

/** What e2e/serve.mjs seeded into the throwaway store, so specs can assert
 *  against exact ids instead of "whatever the first row happens to be". */
export interface SeedState {
  origin: string;
  workspace: string;
  storeDir: string;
  /** Run of bots/demo-bot (tool → compute → done), status finished. */
  fixtureRunId: string;
  /** Run of bots/preview-bot, which emitted a preview_url_available event. */
  previewRunId: string;
  previewUrl: string;
  /** Native kanban card seeded in the `inbox` state. */
  issueId: string;
  maxConcurrentPipelines: number;
}

let cached: SeedState | null = null;

export function seed(): SeedState {
  if (cached) return cached;
  const file = path.join(here, "..", ".tmp", "workspace", "state.json");
  if (!fs.existsSync(file)) {
    throw new Error(
      `seed state missing at ${file} — e2e/serve.mjs must run first (it is the Playwright webServer)`,
    );
  }
  cached = JSON.parse(fs.readFileSync(file, "utf8")) as SeedState;
  return cached;
}

/** Absolute path inside the seeded workspace. */
export function wsPath(...parts: string[]): string {
  return path.join(seed().workspace, ...parts);
}
