import { errorMessage } from "@/lib/errorHints";
import { useCallback, useMemo, useRef } from "react";
import { useQuery } from "@tanstack/react-query";

import { fetchModels, type ModelCatalog, type ModelEntry } from "@/api/models";

// Stable empty fallback so the undefined→loaded transition doesn't hand every
// downstream useMemo a fresh [] reference each render.
const EMPTY_MODELS: ModelEntry[] = [];
const EMPTY_INVALID: { spec: string; reason: string }[] = [];

// The catalog is host state (credentials + a 24h-cached spec aggregator), not
// live data. Cache it for the session and let the explicit refresh path be the
// only thing that re-probes.
const STALE_MS = 5 * 60 * 1000;

export interface UseModelCatalogOptions {
  // Extra specs to resolve IN ADDITION to the curated set — the DSL defaults
  // of the bot's own nodes, which may sit outside it. They widen the catalog,
  // never narrow it.
  extraSpecs?: string[];
  enabled?: boolean;
}

export interface UseModelCatalogResult {
  models: ModelEntry[];
  // bySpec is the lookup a picker needs to answer "what did the operator just
  // choose" without scanning the list on every keystroke.
  bySpec: Map<string, ModelEntry>;
  recommended: ModelEntry | null;
  resolvedDefaultBackend: string;
  // Specs the registry could not resolve. Surfaced rather than swallowed:
  // a node whose DSL default is malformed silently vanishes from the list
  // otherwise, and "it isn't in the picker" reads as "I can't use it".
  invalidSpecs: { spec: string; reason: string }[];
  catalog: ModelCatalog | null;
  loading: boolean;
  error: string | null;
  refresh: () => void;
}

export function useModelCatalog(
  opts: UseModelCatalogOptions = {},
): UseModelCatalogResult {
  const enabled = opts.enabled !== false;
  // Sort + dedupe so two callers asking for the same specs in a different
  // order share one cache entry instead of thrashing it.
  const extraSpecs = useMemo(
    () => [...new Set((opts.extraSpecs ?? []).filter(Boolean))].sort(),
    [opts.extraSpecs],
  );

  // `refresh()` has to re-probe, not just re-read. react-query's refetch
  // re-runs the SAME queryFn, so threading the flag through a ref is what
  // makes the button mean "ask the host again" rather than "return the
  // cached detect.Report you already had". Not a distinct query key on
  // purpose: a forced refresh must replace the cached answer, not sit
  // beside it.
  const forceRef = useRef(false);
  const refreshGenerationRef = useRef(0);
  const query = useQuery({
    queryKey: ["models", extraSpecs],
    queryFn: ({ signal }) => {
      const refresh = forceRef.current;
      return fetchModels({ extraSpecs, refresh, signal });
    },
    enabled,
    staleTime: STALE_MS,
  });
  const refetch = query.refetch;
  const refresh = useCallback(() => {
    const generation = ++refreshGenerationRef.current;
    forceRef.current = true;
    // Keep refresh=true for the whole react-query retry cycle. Clearing it
    // inside queryFn made a failed forced request retry as an ordinary cached
    // read while the UI reported that refresh had succeeded.
    void refetch().finally(() => {
      if (refreshGenerationRef.current === generation) forceRef.current = false;
    });
  }, [refetch]);

  const catalog = query.data ?? null;
  const models = catalog?.models ?? EMPTY_MODELS;

  const bySpec = useMemo(() => {
    const m = new Map<string, ModelEntry>();
    for (const e of models) m.set(e.spec, e);
    return m;
  }, [models]);

  const recommended = useMemo(() => {
    const spec = catalog?.recommended_spec;
    if (spec) return bySpec.get(spec) ?? null;
    return models.find((m) => m.recommended) ?? null;
  }, [catalog?.recommended_spec, bySpec, models]);

  return {
    models,
    bySpec,
    recommended,
    resolvedDefaultBackend: catalog?.resolved_default_backend ?? "",
    invalidSpecs: catalog?.invalid_specs ?? EMPTY_INVALID,
    catalog,
    loading: query.isLoading,
    error: query.isError ? errorMessage(query.error) : null,
    refresh,
  };
}
