import { errorMessage } from "@/lib/errorHints";
import { readJSONFlag, writeJSONFlag, removeFlag } from "@/lib/localStorageFlag";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { getIssue, type NativeIssue } from "@/api/native";
import type { RunEvent } from "@/api/runs";
import { useRunStore } from "@/store/run";

const POLL_INTERVAL_MS = 15_000;

const STORAGE_PREFIX_OBSERVED = "iterion.watchlist.observed:";
const STORAGE_PREFIX_PENDING = "iterion.watchlist.pending:";

export interface WatchEntry {
  issueId: string;
  issue: NativeIssue | null;
  lastFetchError?: string;
}

export interface WatchUpdate {
  issueId: string;
  title: string;
  prevState: string;
  newState: string;
  at: string;
}

export interface UseWatchListResult {
  entries: WatchEntry[];
  pendingUpdates: WatchUpdate[];
  acknowledgeUpdates: () => void;
}

// deriveWatchedIds — the watch-list source of truth is the
// server-authoritative Run.WatchedIssueIDs, stamped from any node
// output carrying `dispatched_ids` (whats-next v2's nexie turn output
// declares it). The v1 event-derived seeding (checkbox answers on the
// removed ask_which_to_* human nodes) is gone — Nexie dispatches
// through board tools and the server stamp covers every path.
export function deriveWatchedIds(
  serverWatched: ReadonlyArray<string> | undefined,
  _events: ReadonlyArray<RunEvent>,
): string[] {
  return dedupeNonEmpty([...(serverWatched ?? [])]);
}

function dedupeNonEmpty(ids: ReadonlyArray<string>): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const id of ids) {
    if (typeof id !== "string" || id.length === 0) continue;
    if (seen.has(id)) continue;
    seen.add(id);
    out.push(id);
  }
  return out;
}

export function useWatchList(runId: string | null): UseWatchListResult {
  const events = useRunStore((s) => s.events);
  // MVP3b: the server-authoritative watch list (Run.WatchedIssueIDs,
  // mirrored into RunHeader). Captures every dispatch path — operator
  // checkbox, LLM dispatch_more, the explicit /watch API — and survives
  // reloads without localStorage. Absent (undefined) for legacy runs
  // persisted before the field existed.
  const serverWatched = useRunStore((s) => s.snapshot?.run?.watched_issue_ids);

  // Stabilize watchedIds reference: a new events array reference on
  // each event push otherwise cascades into a new watchedIds array →
  // re-fires every downstream effect/memo even when the dispatched
  // set is unchanged.
  const watchedIdsRef = useRef<string[]>([]);
  const watchedIds = useMemo(() => {
    const next = deriveWatchedIds(serverWatched, events);
    const prev = watchedIdsRef.current;
    if (prev.length === next.length && prev.every((v, i) => v === next[i])) {
      return prev;
    }
    watchedIdsRef.current = next;
    return next;
  }, [events, serverWatched]);

  const [byId, setById] = useState<Record<string, WatchEntry>>({});
  const [pendingUpdates, setPendingUpdates] = useState<WatchUpdate[]>(() =>
    runId ? loadJSON(STORAGE_PREFIX_PENDING + runId, validateUpdates, []) : [],
  );

  const lastObservedRef = useRef<Map<string, string>>(new Map());
  useEffect(() => {
    // Tear down the previous run's pollers on ANY runId change (incl. →null):
    // a stale fetchOnce closure captured the OLD runId but reads the ref we
    // reassign below, so it would persist the new run's observed-map under the
    // OLD run's localStorage key (and keep polling the wrong run). This effect
    // runs before the poll-setup effect, so every interval is recreated fresh.
    const pollers = pollersRef.current;
    for (const timer of pollers.values()) clearInterval(timer);
    pollers.clear();
    if (!runId) return;
    lastObservedRef.current = loadObservedMap(runId);
    setPendingUpdates(loadJSON(STORAGE_PREFIX_PENDING + runId, validateUpdates, []));
    setById({});
  }, [runId]);

  // Persist pendingUpdates explicitly at the call sites that mutate
  // them (transition push + acknowledge) instead of via an effect on
  // [runId, pendingUpdates] — the effect-based variant raced the
  // hydrate effect on every runId switch, clobbering the new run's
  // buffer with the previous run's state for one tick.
  const persistPending = useCallback((updates: WatchUpdate[]) => {
    if (!runId) return;
    if (updates.length === 0) removeFlag(STORAGE_PREFIX_PENDING + runId);
    else writeJSONFlag(STORAGE_PREFIX_PENDING + runId, updates);
  }, [runId]);

  const pollersRef = useRef<Map<string, ReturnType<typeof setInterval>>>(
    new Map(),
  );

  useEffect(() => {
    if (!runId) return;
    const pollers = pollersRef.current;

    for (const id of watchedIds) {
      if (pollers.has(id)) continue;
      setById((prev) =>
        prev[id] ? prev : { ...prev, [id]: { issueId: id, issue: null } },
      );

      const fetchOnce = async () => {
        try {
          const iss = await getIssue(id);
          const last = lastObservedRef.current.get(id);
          if (last !== undefined && last !== iss.state) {
            setPendingUpdates((prev) => {
              const next = [
                ...prev,
                {
                  issueId: id,
                  title: iss.title,
                  prevState: last,
                  newState: iss.state,
                  at: iss.updated_at,
                },
              ];
              persistPending(next);
              return next;
            });
          }
          if (last !== iss.state) {
            lastObservedRef.current.set(id, iss.state);
            persistObservedMap(runId, lastObservedRef.current);
          }
          setById((prev) => {
            const existing = prev[id];
            if (
              existing &&
              existing.issue &&
              existing.issue.updated_at === iss.updated_at &&
              !existing.lastFetchError
            ) {
              return prev;
            }
            return { ...prev, [id]: { issueId: id, issue: iss } };
          });
        } catch (e) {
          const msg = errorMessage(e) ?? String(e);
          setById((prev) => {
            const existing = prev[id];
            if (existing?.lastFetchError === msg) return prev;
            return {
              ...prev,
              [id]: {
                issueId: id,
                issue: existing?.issue ?? null,
                lastFetchError: msg,
              },
            };
          });
        }
      };
      void fetchOnce();
      pollers.set(id, setInterval(fetchOnce, POLL_INTERVAL_MS));
    }

    for (const [id, timer] of pollers) {
      if (watchedIds.includes(id)) continue;
      clearInterval(timer);
      pollers.delete(id);
      lastObservedRef.current.delete(id);
      persistObservedMap(runId, lastObservedRef.current);
    }
  }, [runId, watchedIds]);

  useEffect(() => {
    const pollers = pollersRef.current;
    return () => {
      for (const timer of pollers.values()) clearInterval(timer);
      pollers.clear();
    };
  }, []);

  const acknowledgeUpdates = useCallback(() => {
    setPendingUpdates([]);
    persistPending([]);
  }, [persistPending]);

  const entries = useMemo(
    () =>
      watchedIds
        .map((id) => byId[id])
        .filter((row): row is WatchEntry => row !== undefined),
    [watchedIds, byId],
  );

  return { entries, pendingUpdates, acknowledgeUpdates };
}

// Thin validation wrapper over the shared readJSONFlag helper (which owns
// the try/catch + missing-key handling). undefined sentinel distinguishes
// "absent" from a stored JSON null so the validator isn't run on absence.
function loadJSON<T>(key: string, validate: (v: unknown) => T | null, fallback: T): T {
  const parsed = readJSONFlag<unknown>(key, undefined);
  return parsed === undefined ? fallback : (validate(parsed) ?? fallback);
}

function loadObservedMap(runId: string): Map<string, string> {
  const obj = loadJSON<Record<string, string>>(
    STORAGE_PREFIX_OBSERVED + runId,
    (v) => (v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, string>) : null),
    {},
  );
  return new Map(Object.entries(obj));
}

function persistObservedMap(runId: string, m: Map<string, string>): void {
  if (m.size === 0) {
    removeFlag(STORAGE_PREFIX_OBSERVED + runId);
    return;
  }
  const obj: Record<string, string> = {};
  for (const [k, v] of m) obj[k] = v;
  writeJSONFlag(STORAGE_PREFIX_OBSERVED + runId, obj);
}

function validateUpdates(v: unknown): WatchUpdate[] | null {
  if (!Array.isArray(v)) return null;
  return v.filter(
    (u): u is WatchUpdate =>
      !!u &&
      typeof u === "object" &&
      typeof (u as WatchUpdate).issueId === "string" &&
      typeof (u as WatchUpdate).title === "string" &&
      typeof (u as WatchUpdate).prevState === "string" &&
      typeof (u as WatchUpdate).newState === "string" &&
      typeof (u as WatchUpdate).at === "string",
  );
}

export function formatUpdatesAsChatMessage(
  updates: ReadonlyArray<WatchUpdate>,
): string {
  if (updates.length === 0) return "";
  const collapsed = new Map<
    string,
    { title: string; prevState: string; newState: string }
  >();
  for (const u of updates) {
    const existing = collapsed.get(u.issueId);
    if (existing) {
      existing.newState = u.newState;
      existing.title = u.title;
    } else {
      collapsed.set(u.issueId, {
        title: u.title,
        prevState: u.prevState,
        newState: u.newState,
      });
    }
  }
  const lines: string[] = ["Board updates since last check:"];
  for (const { title, prevState, newState } of collapsed.values()) {
    lines.push(`- ${title}: ${prevState} → ${newState}`);
  }
  return lines.join("\n");
}
