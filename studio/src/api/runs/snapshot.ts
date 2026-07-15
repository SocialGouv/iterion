// Extracted from api/runs.ts to keep that file focused.
// Single-run reads: snapshot, paginated events, and tool I/O sidecar
// streaming (the only direct-fetch endpoint in the runs barrel).

import { is404 } from "@/api/client";
import { BASE_URL, extractErrorMessage, request, withStoreParam } from "./client";
import type {
  RunEvent,
  RunSnapshot,
  SessionBoardSpec,
  ToolBlobChunk,
} from "./types";

// getSessionBoard fetches the LLM-curated Session-board spec for a run
// (the widgets shown beneath the task list on the Tasks tab). The server
// returns a zero-value spec (version 0, no widgets) when curation never
// ran — never a 404 — so the caller renders the task board alone.
export async function getSessionBoard(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<SessionBoardSpec> {
  const qs = withStoreParam(new URLSearchParams()).toString();
  return request(
    `/runs/${encodeURIComponent(runId)}/session-board${qs ? `?${qs}` : ""}`,
    { signal: opts?.signal },
  );
}

export async function getRun(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<RunSnapshot> {
  const qs = withStoreParam(new URLSearchParams()).toString();
  return request(
    `/runs/${encodeURIComponent(runId)}${qs ? `?${qs}` : ""}`,
    { signal: opts?.signal },
  );
}

// getRunWithRetry wraps getRun in a short backoff loop. The launch API
// returns the run_id as soon as the engine goroutine is scheduled, but
// the goroutine still needs a beat to call store.CreateRun before
// run.json exists on disk — fetching too early therefore 404s. run.json
// typically lands within ~50–200ms, so a few 250ms retries close the
// race without papering over a genuinely missing run (the 404 budget is
// deliberately small); transient non-404 failures get a longer budget.
//
// Cancellation: pass an AbortSignal. Aborting cancels both the
// in-flight request and any pending retry delay; the promise then
// rejects with an "AbortError"-named error, which callers should treat
// as "stop silently" (the same contract as a plain aborted getRun).
// On retry exhaustion the LAST error is rethrown unchanged, so callers
// can still classify it (see is404 in @/api/client).
export async function getRunWithRetry(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<RunSnapshot> {
  const signal = opts?.signal;
  const RETRY_DELAY_MS = 250;
  const MAX_ATTEMPTS_404 = 3;
  const MAX_ATTEMPTS_OTHER = 20;
  let attempt = 0;
  for (;;) {
    try {
      return await getRun(runId, { signal });
    } catch (err) {
      if (signal?.aborted || (err as Error)?.name === "AbortError") {
        throw err;
      }
      attempt += 1;
      const cap = is404(err) ? MAX_ATTEMPTS_404 : MAX_ATTEMPTS_OTHER;
      if (attempt >= cap) throw err;
      await abortableDelay(RETRY_DELAY_MS, signal);
    }
  }
}

// abortableDelay resolves after ms, or rejects with an AbortError as
// soon as the signal fires — so an aborted retry loop stops immediately
// instead of ticking against the network for its whole budget.
function abortableDelay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(makeAbortError());
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    function onAbort() {
      clearTimeout(timer);
      reject(makeAbortError());
    }
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

function makeAbortError(): Error {
  // DOMException matches what fetch itself throws on abort, keeping a
  // single detection idiom (`err.name === "AbortError"`) for callers.
  if (typeof DOMException !== "undefined") {
    return new DOMException("The operation was aborted.", "AbortError");
  }
  const err = new Error("The operation was aborted.");
  err.name = "AbortError";
  return err;
}

export async function loadEvents(
  runId: string,
  from = 0,
  to = 0,
): Promise<RunEvent[]> {
  const qs = new URLSearchParams();
  if (from > 0) qs.set("from", String(from));
  if (to > 0) qs.set("to", String(to));
  withStoreParam(qs);
  const suffix = qs.toString();
  const res = await request<{ events: RunEvent[] }>(
    `/runs/${encodeURIComponent(runId)}/events${suffix ? `?${suffix}` : ""}`,
  );
  return res.events ?? [];
}

// fetchToolBlob streams a slice of a tool's stored I/O sidecar (written
// by the backend hooks layer when an input/output exceeded the inline
// threshold). offset is the byte offset to start at; limit caps bytes
// returned (0 = "all from offset"). Returns the bytes as a UTF-8 string
// plus the full size and an eof flag so the UI can keep fetching until
// the end. Throws on network / status errors; a 404 means the call's
// payload fit inline (no sidecar) — callers should fall back to the
// preview field in that case.
export async function fetchToolBlob(
  runId: string,
  toolUseID: string,
  kind: "input" | "output",
  offset = 0,
  limit = 0,
): Promise<ToolBlobChunk> {
  const qs = new URLSearchParams();
  if (offset > 0) qs.set("offset", String(offset));
  if (limit > 0) qs.set("limit", String(limit));
  const suffix = qs.toString();
  const url = `${BASE_URL}/runs/${encodeURIComponent(runId)}/tools/${encodeURIComponent(toolUseID)}/${kind}${suffix ? `?${suffix}` : ""}`;
  const res = await fetch(url, { credentials: "include" });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await extractErrorMessage(res)}`);
  }
  const data = await res.text();
  const totalHeader = res.headers.get("X-Tool-Total-Size") ?? "0";
  const total = Number.parseInt(totalHeader, 10) || data.length;
  const eofHeader = res.headers.get("X-Tool-Eof") ?? "";
  const eof = eofHeader === "true";
  return { data, total, eof };
}
