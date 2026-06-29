// Extracted from api/runs.ts to keep that file focused.
// Single-run reads: snapshot, paginated events, and tool I/O sidecar
// streaming (the only direct-fetch endpoint in the runs barrel).

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
