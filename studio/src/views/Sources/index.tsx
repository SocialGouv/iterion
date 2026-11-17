// The source catalog (#3272): what an operator could plug, read from
// GET /api/v1/sources. Search and category pills live in the URL, an open
// detail drawer does too (?source=<id>), so a deep link reopens the same
// state.
import { useQuery } from "@tanstack/react-query";
import { useSearch, useLocation, Link } from "wouter";
import { useMemo } from "react";

import { Button } from "@/components/ui/Button";
import { Card } from "@/components/ui/Card";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Spinner } from "@/components/ui/Spinner";
import { SourceCard } from "./SourceCard";
import { SourceDetail } from "./SourceDetail";
import {
  listSources,
  sourceCategoryLabel,
  FeatureUnavailableError,
} from "@/api/sources";

// Where each built-in family plugs in: the dialogs the Automations view
// already has. The catalog card routes there rather than re-implementing
// one (views are additive, philosophy §5).
const BUILTIN_TARGETS: Record<string, string> = {
  schedule: "/triggers?tab=schedules",
  board: "/triggers",
  run_outcome: "/triggers",
  emit: "/triggers",
};

export default function SourcesView() {
  const search = useSearch();
  const [, navigate] = useLocation();
  const params = useMemo(() => new URLSearchParams(search), [search]);
  const q = params.get("q") ?? "";
  const category = params.get("category") ?? "";
  const openId = params.get("source") ?? "";

  const sources = useQuery({
    queryKey: ["sources"],
    queryFn: listSources,
  });

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    navigate(`/sources${next.size ? `?${next}` : ""}`, { replace: true });
  };

  const connectors = useMemo(
    () => (sources.data ?? []).filter((s) => s.kind === "connector"),
    [sources.data],
  );
  const builtins = useMemo(
    () => (sources.data ?? []).filter((s) => s.kind === "builtin"),
    [sources.data],
  );
  const categories = useMemo(
    () => [...new Set(connectors.map((s) => s.category ?? ""))].sort(),
    [connectors],
  );
  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return connectors.filter((s) => {
      if (category && (s.category ?? "") !== category) return false;
      if (!needle) return true;
      return (
        s.display_name.toLowerCase().includes(needle) ||
        s.id.toLowerCase().includes(needle) ||
        (s.description ?? "").toLowerCase().includes(needle)
      );
    });
  }, [connectors, q, category]);

  const open = connectors.find((s) => s.id === openId) ?? null;

  if (sources.isPending) {
    return (
      <div className="flex justify-center py-16">
        <Spinner label="Chargement du catalogue" />
      </div>
    );
  }
  if (sources.isError) {
    if (sources.error instanceof FeatureUnavailableError) {
      return (
        <InlineBanner tone="info">
          Le catalogue de sources n'est pas disponible sur ce serveur.
        </InlineBanner>
      );
    }
    return (
      <InlineBanner tone="danger">
        Erreur de chargement du catalogue : {String(sources.error)}
      </InlineBanner>
    );
  }

  return (
    <div className="mx-auto w-full max-w-5xl p-4">
      <div className="flex items-baseline justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-fg-default">Sources</h1>
          <p className="text-xs text-fg-subtle mt-0.5">
            Les événements qu'un bot peut écouter — connecteurs du catalogue
            et familles intégrées.
          </p>
        </div>
        <div className="w-56">
          <Input
            type="search"
            value={q}
            onChange={(e) => setParam("q", e.target.value)}
            placeholder="Rechercher une source…"
            aria-label="Rechercher une source"
          />
        </div>
      </div>

      {categories.length > 0 && (
        <div className="mt-3 flex flex-wrap gap-1.5" role="group" aria-label="Filtrer par catégorie">
          <button
            type="button"
            onClick={() => setParam("category", "")}
            aria-pressed={category === ""}
            className={`rounded-full border px-2.5 py-0.5 text-xs transition-colors focus:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
              category === ""
                ? "border-accent bg-accent-soft text-fg-default"
                : "border-border-default bg-surface-1 text-fg-muted hover:bg-surface-2 hover:text-fg-default"
            }`}
          >
            Toutes
          </button>
          {categories.map((c) => (
            <button
              key={c}
              type="button"
              onClick={() => setParam("category", c)}
              aria-pressed={category === c}
              className={`rounded-full border px-2.5 py-0.5 text-xs transition-colors focus:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
                category === c
                  ? "border-accent bg-accent-soft text-fg-default"
                  : "border-border-default bg-surface-1 text-fg-muted hover:bg-surface-2 hover:text-fg-default"
              }`}
            >
              {sourceCategoryLabel(c || undefined)}
            </button>
          ))}
        </div>
      )}

      {shown.length === 0 && builtins.length === 0 ? (
        <EmptyState
          title="Aucune source"
          message="Aucun paquet de connecteur ne déclare d'événements sur ce serveur."
        />
      ) : (
        <div className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {shown.map((s) => (
            <SourceCard key={s.id} source={s} onOpen={() => setParam("source", s.id)} />
          ))}
          {builtins.map((s) => (
            <Card key={s.id} className="flex flex-col gap-1">
              <div className="flex items-center gap-2">
                <span
                  aria-hidden
                  className="flex h-8 w-8 items-center justify-center rounded-[var(--radius-md)] bg-accent-soft text-xs font-semibold text-fg-default"
                >
                  {s.display_name.slice(0, 1)}
                </span>
                <span className="text-sm font-medium text-fg-default">{s.display_name}</span>
              </div>
              <p className="text-xs text-fg-subtle">{s.description}</p>
              <div className="mt-auto pt-2">
                <Link href={BUILTIN_TARGETS[s.id] ?? "/triggers"}>
                  <Button
                    variant="secondary"
                    size="sm"
                    aria-label={`Configurer ${s.display_name}`}
                  >
                    Configurer
                  </Button>
                </Link>
              </div>
            </Card>
          ))}
        </div>
      )}

      <SourceDetail source={open} onClose={() => setParam("source", "")} />
    </div>
  );
}
