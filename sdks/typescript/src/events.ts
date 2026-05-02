/**
 * Event tailer for `<store>/runs/<runId>/events.jsonl`.
 *
 * Reads existing events; if `follow: true`, watches for appended lines
 * via `fs.watch` (or polling on filesystems where `fs.watch` is
 * unreliable, e.g. NFS, Docker bind mounts).
 *
 * Handles partial-line buffering: an event line written between two
 * reads can be split across reads, so we accumulate until we see `\n`.
 */

import { open, stat, watch } from "node:fs/promises";
import type { FileHandle } from "node:fs/promises";

import { eventsPath } from "./paths.js";
import type { Event } from "./types.js";

export interface TailEventsOptions {
  /** Run store directory (default: ".iterion"). */
  storeDir?: string;
  /** Continue watching the file for new events. Default: false. */
  follow?: boolean;
  /** Use polling instead of `fs.watch` (e.g. for NFS / FUSE). Default: false. */
  polling?: boolean;
  /** Polling interval in ms when `polling=true`. Default: 250. */
  pollIntervalMs?: number;
  /** Abort the tail loop. */
  signal?: AbortSignal;
}

const READ_CHUNK_SIZE = 64 * 1024;

/**
 * Yield events from a run's events.jsonl. With `follow: true` the
 * iterator stays open and yields new events as they are appended.
 */
export async function* tailEvents(
  runId: string,
  opts: TailEventsOptions = {},
): AsyncGenerator<Event, void, unknown> {
  const path = eventsPath(runId, opts.storeDir);
  const signal = opts.signal;
  // Per-tail read buffer; reused across `readNew` calls to avoid
  // re-allocating 64 KiB on every watcher tick.
  const readBuf = Buffer.alloc(READ_CHUNK_SIZE);

  let handle: FileHandle | null = null;
  try {
    handle = await open(path, "r");
  } catch (err) {
    if (!opts.follow) {
      throw err;
    }
    // In follow mode, wait for the file to appear.
    handle = await waitForFile(path, opts, signal);
  }

  let offset = 0;
  let buffer = "";

  try {
    while (!signal?.aborted) {
      const before = offset;
      const result = await readNew(handle, offset, readBuf);
      offset = result.nextOffset;
      buffer += result.text;

      let nl = buffer.indexOf("\n");
      while (nl !== -1) {
        const raw = buffer.slice(0, nl);
        buffer = buffer.slice(nl + 1);
        const trimmed = raw.replace(/\r$/, "").trim();
        if (trimmed) {
          try {
            yield JSON.parse(trimmed) as Event;
          } catch {
            // Tolerate corrupt/partial trailing lines on log rotation.
          }
        }
        nl = buffer.indexOf("\n");
      }

      if (!opts.follow) {
        return;
      }

      const grew = offset > before;
      if (!grew) {
        await waitForChange(path, opts, signal);
        if (signal?.aborted) return;
        // Re-stat — if the file shrank (truncation/rotation), reset.
        try {
          const st = await stat(path);
          if (st.size < offset) {
            offset = 0;
            buffer = "";
            await handle.close().catch(() => undefined);
            handle = await open(path, "r");
          }
        } catch {
          // Ignore stat failures — next read iteration will retry.
        }
      }
    }
  } finally {
    await handle?.close().catch(() => undefined);
  }
}

async function readNew(
  handle: FileHandle,
  offset: number,
  buf: Buffer,
): Promise<{ text: string; nextOffset: number }> {
  const chunkSize = buf.length;
  let text = "";
  let pos = offset;
  // Drain the file in chunks until we hit EOF for this iteration.
  // Looping handles writers that dump multiple events between watch events.
  for (;;) {
    const { bytesRead } = await handle.read(buf, 0, chunkSize, pos);
    if (bytesRead <= 0) break;
    text += buf.slice(0, bytesRead).toString("utf8");
    pos += bytesRead;
    if (bytesRead < chunkSize) break;
  }
  return { text, nextOffset: pos };
}

async function waitForFile(
  path: string,
  opts: TailEventsOptions,
  signal: AbortSignal | undefined,
): Promise<FileHandle> {
  const interval = Math.max(50, opts.pollIntervalMs ?? 250);
  while (!signal?.aborted) {
    try {
      return await open(path, "r");
    } catch {
      await sleep(interval, signal);
    }
  }
  throw new AbortError();
}

async function waitForChange(
  path: string,
  opts: TailEventsOptions,
  signal: AbortSignal | undefined,
): Promise<void> {
  const interval = Math.max(50, opts.pollIntervalMs ?? 250);
  if (opts.polling) {
    await sleep(interval, signal);
    return;
  }
  // fs.watch returns an async iterable; we wait for one event then
  // resume reading. The iterator is closed via `signal`.
  try {
    const ac = new AbortController();
    const onAbort = () => ac.abort();
    signal?.addEventListener("abort", onAbort, { once: true });
    try {
      const watcher = watch(path, { signal: ac.signal });
      // We only need one tick to know "go look again".
      const next = await raceWithTimeout(
        watcher[Symbol.asyncIterator]().next(),
        interval * 4,
      );
      void next;
    } finally {
      ac.abort();
      signal?.removeEventListener("abort", onAbort);
    }
  } catch {
    // fs.watch can throw on some filesystems — fall back to a sleep.
    await sleep(interval, signal);
  }
}

async function raceWithTimeout<T>(p: Promise<T>, timeoutMs: number): Promise<T | null> {
  let timeout: NodeJS.Timeout | null = null;
  const timer = new Promise<null>((resolveTimer) => {
    timeout = setTimeout(() => resolveTimer(null), timeoutMs);
    timeout.unref?.();
  });
  try {
    return await Promise.race([p, timer]);
  } finally {
    if (timeout) clearTimeout(timeout);
  }
}

function sleep(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolveSleep) => {
    const timer = setTimeout(resolveSleep, ms);
    timer.unref?.();
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolveSleep();
      },
      { once: true },
    );
  });
}

class AbortError extends Error {
  constructor() {
    super("aborted");
    this.name = "AbortError";
  }
}
