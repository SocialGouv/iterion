import { errorMessage } from "@/lib/errorHints";
import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";

import { fetchModels, type ModelCatalog, type ModelEntry } from "@/api/models";

// Stable empty fallback so the undefined→loaded transition doesn't hand every
// downstream useMemo a fresh [] reference each render.
const EMPTY_MODELS: ModelEntry[] = [];

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

  const query = useQuery({
    queryKey: ["models", extraSpecs],
    queryFn: ({ signal }) => fetchModels({ extraSpecs, signal }),
    enabled,
    staleTime: STALE_MS,
  });

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
    catalog,
    loading: query.isLoading,
    error: query.isError ? errorMessage(query.error) : null,
    refresh: () => void query.refetch(),
  };
}
