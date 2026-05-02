import { readFile } from "node:fs/promises";
import { describe, expect, it } from "vitest";

import { partitionAnswers, writeAnswersFile } from "../src/index.js";

describe("partitionAnswers", () => {
  it("splits string values from non-string values", () => {
    const { flagAnswers, fileAnswers } = partitionAnswers({
      a: "yes",
      b: true,
      c: 42,
      d: { nested: "ok" },
      e: [1, 2, 3],
    });
    expect(flagAnswers).toEqual({ a: "yes" });
    expect(fileAnswers).toEqual({ b: true, c: 42, d: { nested: "ok" }, e: [1, 2, 3] });
  });

  it("returns empty maps for empty input", () => {
    expect(partitionAnswers({})).toEqual({ flagAnswers: {}, fileAnswers: {} });
  });
});

describe("writeAnswersFile", () => {
  it("round-trips JSON content and cleans up the temp dir", async () => {
    const written = await writeAnswersFile({ approve: true, score: 0.9 });
    try {
      const raw = await readFile(written.path, "utf8");
      expect(JSON.parse(raw)).toEqual({ approve: true, score: 0.9 });
    } finally {
      await written.cleanup();
    }
    // Second cleanup is a no-op
    await written.cleanup();
  });
});
