/**
 * Helper for building answer payloads when resuming a paused run.
 *
 * The CLI exposes two flags for answers:
 *   --answer key=value   (string-only, repeatable)
 *   --answers-file PATH  (full JSON map, supports any value type)
 *
 * Object/array/number/boolean answers can only travel through the JSON
 * file path, so we materialise a temp file when needed.
 */

import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { JsonValue } from "./types.js";

export interface AnswersFile {
  /** Absolute path of the written JSON file. */
  path: string;
  /** Removes the temp directory holding the file. Safe to call twice. */
  cleanup: () => Promise<void>;
}

/**
 * Write `answers` to a temporary JSON file. Caller is responsible for
 * invoking `cleanup()` once the resume has completed (typically in a
 * `finally` block).
 */
export async function writeAnswersFile(
  answers: Record<string, JsonValue>,
  opts: { tmpDir?: string } = {},
): Promise<AnswersFile> {
  const baseDir = opts.tmpDir ?? tmpdir();
  const dir = await mkdtemp(join(baseDir, "iterion-sdk-answers-"));
  const path = join(dir, "answers.json");
  await writeFile(path, JSON.stringify(answers), "utf8");
  let removed = false;
  return {
    path,
    cleanup: async () => {
      if (removed) return;
      removed = true;
      await rm(dir, { recursive: true, force: true });
    },
  };
}

/**
 * Split a flat answers map into the subset that fits the `--answer
 * key=value` flag (strings only) and the subset that requires a file.
 */
export function partitionAnswers(answers: Record<string, JsonValue>): {
  flagAnswers: Record<string, string>;
  fileAnswers: Record<string, JsonValue>;
} {
  const flagAnswers: Record<string, string> = {};
  const fileAnswers: Record<string, JsonValue> = {};
  for (const [k, v] of Object.entries(answers)) {
    if (typeof v === "string") {
      flagAnswers[k] = v;
    } else {
      fileAnswers[k] = v;
    }
  }
  return { flagAnswers, fileAnswers };
}
