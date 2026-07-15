import { useCallback, useMemo } from "react";
import { useQueries, type UseQueryResult } from "@tanstack/react-query";
import type { IterDocument } from "@/api/types";
import { openFile } from "@/api/client";
import { resolveSubbotSource, type SubbotChildDoc } from "@/lib/subbotGraph";

// Loads the child .bot document of every subbot declaration in the open
// file, keyed by subbot name, for expandSubbots. Each distinct resolved
// path is fetched once (react-query dedupes by key); staleTime keeps the
// graph steady while still picking up child-file edits within ~15s.
const CHILD_DOC_STALE_MS = 15_000;

const EMPTY_MAP = new Map<string, SubbotChildDoc>();

type OpenFileResult = Awaited<ReturnType<typeof openFile>>;

export function useSubbotDocuments(
  document: IterDocument | null,
  filePath: string | null,
): Map<string, SubbotChildDoc> {
  // name -> resolved workspace-relative path (subbots without a source are
  // skipped: nothing to load, the compact node just stays).
  const pathByName = useMemo(() => {
    const m = new Map<string, string>();
    for (const sb of document?.subbots ?? []) {
      if (!sb.source) continue;
      m.set(sb.name, resolveSubbotSource(filePath, sb.source));
    }
    return m;
  }, [document?.subbots, filePath]);

  const distinctPaths = useMemo(
    () => Array.from(new Set(pathByName.values())).sort(),
    [pathByName],
  );

  // Stable combine (react-query only memoizes the combined value when the
  // combine fn itself is referentially stable across renders) — identity of
  // the resulting Map changes only when a child query result changes, so the
  // canvas layout memo doesn't churn on unrelated renders.
  const combine = useCallback(
    (results: UseQueryResult<OpenFileResult, Error>[]) => {
      const m = new Map<string, { doc?: IterDocument; error?: string }>();
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

  return useMemo(() => {
    if (pathByName.size === 0) return EMPTY_MAP;
    const out = new Map<string, SubbotChildDoc>();
    for (const [name, path] of pathByName) {
      const res = byPath.get(path);
      out.set(name, { path, doc: res?.doc, error: res?.error });
    }
    return out;
  }, [pathByName, byPath]);
}
