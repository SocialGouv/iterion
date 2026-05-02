import { appendFile, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { tailEvents } from "../src/index.js";
import type { Event } from "../src/index.js";
import { makeTmpStore, type TmpStore } from "./helpers/tmpStore.js";

describe("tailEvents", () => {
  let store: TmpStore;
  let eventsFile: string;

  beforeEach(async () => {
    store = await makeTmpStore("iterion-sdk-events-");
    const runDir = await store.ensureRunDir("run_1");
    eventsFile = join(runDir, "events.jsonl");
  });

  afterEach(async () => {
    await store.cleanup();
  });

  function evt(seq: number, type: Event["type"]): Event {
    return {
      seq,
      timestamp: new Date(2026, 0, 1, 0, 0, seq).toISOString(),
      type,
      run_id: "run_1",
    };
  }

  it("yields all existing events when not following", async () => {
    const lines = [evt(0, "run_started"), evt(1, "node_started"), evt(2, "run_finished")]
      .map((e) => JSON.stringify(e))
      .join("\n");
    await writeFile(eventsFile, lines + "\n");

    const out: Event[] = [];
    for await (const e of tailEvents("run_1", { storeDir: store.storeDir })) {
      out.push(e);
    }
    expect(out.map((e) => e.type)).toEqual([
      "run_started",
      "node_started",
      "run_finished",
    ]);
  });

  it("ignores corrupt trailing lines", async () => {
    const good = JSON.stringify(evt(0, "run_started"));
    await writeFile(eventsFile, good + "\n{not json}\n");
    const out: Event[] = [];
    for await (const e of tailEvents("run_1", { storeDir: store.storeDir })) {
      out.push(e);
    }
    expect(out).toHaveLength(1);
    expect(out[0]!.type).toBe("run_started");
  });

  it("rejects traversal in runId before opening escaped events files", async () => {
    await mkdir(join(store.tmp, "secret"), { recursive: true });
    await writeFile(join(store.tmp, "secret", "events.jsonl"), `${JSON.stringify(evt(0, "run_started"))}\n`);

    await expect(async () => {
      for await (const _ of tailEvents("../../secret", { storeDir: store.storeDir })) {
        // should reject before yielding or reading the escaped file
      }
    }).rejects.toThrow(/path traversal|path separator|safe path component/);

    await expect(async () => {
      for await (const _ of tailEvents("..\\..\\secret", { storeDir: store.storeDir })) {
        // should reject before yielding or reading the escaped file
      }
    }).rejects.toThrow(/path traversal|path separator|safe path component/);
  });

  it("buffers partial-line writes when following", async () => {
    await writeFile(eventsFile, "");

    const ac = new AbortController();
    const seen: Event[] = [];
    const collector = (async () => {
      for await (const e of tailEvents("run_1", {
        storeDir: store.storeDir,
        follow: true,
        polling: true,
        pollIntervalMs: 50,
        signal: ac.signal,
      })) {
        seen.push(e);
        if (seen.length >= 2) {
          ac.abort();
          break;
        }
      }
    })();

    // Write the first half of an event line, then the rest a moment later.
    const first = JSON.stringify(evt(0, "run_started"));
    const second = JSON.stringify(evt(1, "run_finished"));
    await appendFile(eventsFile, first.slice(0, 10));
    await new Promise((r) => setTimeout(r, 100));
    await appendFile(eventsFile, first.slice(10) + "\n");
    await new Promise((r) => setTimeout(r, 100));
    await appendFile(eventsFile, second + "\n");

    await collector;
    expect(seen.map((e) => e.type)).toEqual(["run_started", "run_finished"]);
  });

  it("terminates when the abort signal fires", async () => {
    await writeFile(eventsFile, "");
    const ac = new AbortController();
    setTimeout(() => ac.abort(), 100);
    const start = Date.now();
    for await (const _ of tailEvents("run_1", {
      storeDir: store.storeDir,
      follow: true,
      polling: true,
      pollIntervalMs: 50,
      signal: ac.signal,
    })) {
      // never enters
    }
    const elapsed = Date.now() - start;
    expect(elapsed).toBeLessThan(2000);
  });
});
