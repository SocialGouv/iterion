import { useCallback, useEffect, useMemo, useState } from "react";
import { useQueries, type UseQueryResult } from "@tanstack/react-query";
import type { IterDocument } from "@/api/types";
import { openFile } from "@/api/client";
import {
  MAX_SUBBOT_EXPANSION_DEPTH,
  resolveSubbotSource,
  type SubbotDocEntry,
} from "@/lib/subbotGraph";

// Loads the child .bot document of every subbot declaration reachable
// from the open file — RECURSIVELY: once a child doc lands, its own
// subbots' sources are discovered and fetched too (BFS over resolved
// paths, bounded by the display depth cap + a visited set, so cycles
// terminate). Keyed by resolved path for expandSubbots; the same file
// referenced from two subbots (or two nesting levels) loads once.
const CHILD_DOC_STALE_MS = 15_000;

const EMPTY_MAP = new Map<string, SubbotDocEntry>();

type OpenFileResult = Awaited<ReturnType<typeof openFile>>;

// discoverPaths walks the subbot graph breadth-first from the root
// document, following into whatever child docs have already loaded.
// Deterministic + bounded: each level only adds unseen resolved paths,
// and the walk stops at the display depth cap.
function discoverPaths(
  document: IterDocument | null,
  filePath: string | null,
  loaded: Map<string, SubbotDocEntry>,
): string[] {
  const seen = new Set<string>();
  const wanted: string[] = [];
  let frontier: Array<{ doc: IterDocument | null; path: string | null }> = [
    { doc: document, path: filePath },
  ];
  for (
    let depth = 0;
    depth < MAX_SUBBOT_EXPANSION_DEPTH && frontier.length > 0;
    depth++
  ) {
    const next: typeof frontier = [];
    for (const { doc, path } of frontier) {
      if (!doc) continue;
      for (const sb of doc.subbots ?? []) {
        if (!sb.source) continue;
        const resolved = resolveSubbotSource(path, sb.source);
        if (resolved === filePath || seen.has(resolved)) continue; // cycle/dup
        seen.add(resolved);
        wanted.push(resolved);
        next.push({ doc: loaded.get(resolved)?.doc ?? null, path: resolved });
      }
    }
    frontier = next;
  }
  return wanted.sort();
}

export function useSubbotDocuments(
  document: IterDocument | null,
  filePath: string | null,
): Map<string, SubbotDocEntry> {
  // The path set grows as child docs land (dependent-query fixed point):
  // paths -> queries -> results -> more paths. State + effect keep the
  // hook order fixed while the set converges (≤ depth-cap iterations;
  // settled queries never refetch, so there is no loop).
  const [distinctPaths, setDistinctPaths] = useState<string[]>([]);

  const combine = useCallback(
    (results: UseQueryResult<OpenFileResult, Error>[]) => {
      const m = new Map<string, SubbotDocEntry>();
      results.forEach((r, i) => {
        const path = distinctPaths[i]!;
        if (r.data) m.set(path, { doc: r.data.document });
        else if (r.error) m.set(path, { error: r.error.message || String(r.error) });
        // still loading: no entry — expandSubbots keeps the compact node
      });
      return m;
    },
    [distinctPaths],
  );

  const byPath = useQueries({
    queries: distinctPaths.map((p) => ({
      queryKey: ["subbot-doc", p],
      queryFn: () => openFile(p),
      staleTime: CHILD_DOC_STALE_MS,
      retry: 1,
    })),
    combine,
  });

  useEffect(() => {
    const wanted = discoverPaths(document, filePath, byPath);
    setDistinctPaths((prev) =>
      prev.length === wanted.length && prev.every((p, i) => p === wanted[i])
        ? prev
        : wanted,
    );
  }, [document, filePath, byPath]);

  return useMemo(() => {
    if (distinctPaths.length === 0) return EMPTY_MAP;
    return byPath;
  }, [distinctPaths, byPath]);
}
