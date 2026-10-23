import { afterEach, describe, expect, it, vi } from "vitest";

import { backoffDelay, startBackoffLoop, type Backoff } from "@/auth/backoffLoop";

const BACKOFF: Backoff = { firstMs: 1000, factor: 2, capMs: 5000 };

afterEach(() => { vi.useRealTimers(); });

describe("backoffDelay", () => {
  it("grows by the factor from the first delay and stops at the cap", () => {
    expect([0, 1, 2, 3, 4, 10].map((n) => backoffDelay(BACKOFF, n))).toEqual([1000, 2000, 4000, 5000, 5000, 5000]);
  });
});

describe("startBackoffLoop", () => {
  it("runs one attempt at a time, each after the backoff, until one is done", async () => {
    vi.useFakeTimers({ now: 0 });
    const starts: number[] = [];
    let ended = 0;
    let answers = 0;
    startBackoffLoop(BACKOFF, async () => {
      starts.push(Date.now());
      // Each attempt takes 300 ms to answer: the next delay counts from then.
      await new Promise((r) => setTimeout(r, 300));
      answers++;
      return answers === 4 ? "done" : "retry";
    }, () => { ended++; });
    await vi.advanceTimersByTimeAsync(60_000);
    expect(starts).toEqual([1000, 3300, 7600, 12900]);
    expect(ended).toBe(1);
  });

  it("re-arms nothing when cancelled while an attempt is in flight, and ends once", async () => {
    vi.useFakeTimers({ now: 0 });
    let attempts = 0;
    let ended = 0;
    let finish: (() => void) | null = null;
    const loop = startBackoffLoop(BACKOFF, () => {
      attempts++;
      return new Promise<"retry">((resolve) => { finish = () => resolve("retry"); });
    }, () => { ended++; });
    await vi.advanceTimersByTimeAsync(1000);
    expect(attempts).toBe(1);
    loop.cancel();
    loop.cancel();
    finish!();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(attempts).toBe(1);
    expect(ended).toBe(1);
  });

  it("never runs a pending attempt once cancelled", async () => {
    vi.useFakeTimers({ now: 0 });
    let attempts = 0;
    const loop = startBackoffLoop(BACKOFF, async () => { attempts++; return "retry"; }, () => {});
    loop.cancel();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(attempts).toBe(0);
  });
});
