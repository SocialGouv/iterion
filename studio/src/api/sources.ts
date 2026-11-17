// Source catalog REST client. Mirrors pkg/server/sources_routes.go
// (/api/v1/sources) and connector_icon.go (the icon route, used as a bare
// <img> src). Hand-mirrored types, like api/triggers.ts: the response
// schemas are inline in schema.ts.

import { apiRequest, guard404, FeatureUnavailableError } from "./client";

export { FeatureUnavailableError };

const BASE = "/api/v1/sources";

// SourceAuthScheme mirrors sourceAuthScheme.
export interface SourceAuthScheme {
  id: string;
  kind: string;
  display_name?: string;
}

// SourceOutput mirrors sourceOutput: a named value a delivery carries.
export interface SourceOutput {
  name: string;
  description?: string;
  type?: string;
}

// SourceFilter mirrors sourceFilter: a value an operator may filter on.
export interface SourceFilter {
  id: string;
  type: string;
  options?: string[];
  default?: string[];
}

// SourceTrigger mirrors sourceTrigger.
export interface SourceTrigger {
  id: string;
  display_name?: string;
  description?: string;
  outputs?: SourceOutput[];
  filters?: SourceFilter[];
}

// Source mirrors sourceEntry — a catalog card.
export interface Source {
  kind: "connector" | "builtin";
  id: string;
  display_name: string;
  description?: string;
  category?: string;
  maturity?: string;
  tier?: string;
  delivery?: "push" | "poll";
  auth?: SourceAuthScheme[];
  triggers?: SourceTrigger[];
  connections: number;
  subscriptions: number;
  // The package's setup.md: what to do at the vendor's side once the
  // binding exists. Plain text, rendered as text.
  setup?: string;
}

export async function listSources(): Promise<Source[]> {
  const r = await guard404("sources", () =>
    apiRequest<{ sources?: Source[] }>(BASE, {}),
  );
  return r.sources ?? [];
}

// sourceIconURL is the icon route for an <img src>: the session cookie
// authenticates it and the answer is always an image — the sanitized mark
// or the monogram — so the component carries no fallback of its own.
export function sourceIconURL(connectorID: string): string {
  return `/api/v1/connectors/${encodeURIComponent(connectorID)}/icon`;
}

// Labels of the closed category taxonomy (pkg/connector/spec Category).
// An unknown category renders raw, so a later taxonomy level stays visible.
export const SOURCE_CATEGORY_LABELS: Record<string, string> = {
  source_control: "Forge & dépôts",
  chat: "Chat",
  issue_tracker: "Suivi de projets",
  incident: "Incidents",
  monitoring: "Supervision",
};

export function sourceCategoryLabel(category?: string): string {
  if (!category) return "Autres";
  return SOURCE_CATEGORY_LABELS[category] ?? category;
}
