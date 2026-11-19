// Source catalog REST client. Mirrors pkg/server/sources_routes.go
// (/api/v1/sources) and connector_icon.go (the icon route, used as a bare
// <img> src). Hand-mirrored types, like api/triggers.ts: the response
// schemas are inline in schema.ts.

import { apiRequest, guard404, FeatureUnavailableError } from "./client";
import { scopePrefix } from "@/lib/scope";

export { FeatureUnavailableError };

const BASE = "/api/v1/sources";

// SourceAuthScheme mirrors sourceAuthScheme. The kind (spec.AuthKind) is
// what the scheme presents: a token (api_key, bearer, oauth2), HTTP basic's
// username and password, or — "none", a public resource — nothing. It stays
// a string: a later kind renders raw rather than refusing the card.
export interface SourceAuthScheme {
  id: string;
  kind: string;
  display_name?: string;
}

// SourceIngress mirrors sourceIngress: the package's delivery contract.
// Absent on a package whose triggers are all polled — nothing is delivered
// to iterion, so there is no URL, no secret, no vendor-side hook.
export interface SourceIngress {
  credential_scheme: string;
  secret_origin: string;
  event_header?: string;
  installation_required: boolean;
}

// SourceDelivery is how triggers reach iterion: a vendor pushes them, or
// iterion polls; a card mixing both says "mixed" and each trigger its own.
export type SourceDelivery = "push" | "poll" | "mixed";

// SourceQueryField mirrors sourceQueryField: one value an operator types for
// a polled trigger's poller. An absent type is a string.
export interface SourceQueryField {
  id: string;
  type?: "string" | "int" | "bool";
  description?: string;
  required?: boolean;
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
  delivery?: "push" | "poll";
  // A polled trigger's query fields, default interval (a Go duration as the
  // package declares it, "5m") and contract (the server's English sentence;
  // the studio words it itself, pollContractText).
  query?: SourceQueryField[];
  interval?: string;
  contract?: string;
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
  delivery?: SourceDelivery;
  auth?: SourceAuthScheme[];
  triggers?: SourceTrigger[];
  connections: number;
  subscriptions: number;
  // The package's setup.md: what to do at the vendor's side once the
  // binding exists. Plain text, rendered as text.
  setup?: string;
  ingress?: SourceIngress;
}

export async function listSources(): Promise<Source[]> {
  const r = await guard404("sources", () =>
    apiRequest<{ sources?: Source[] }>(BASE, {}),
  );
  return r.sources ?? [];
}

// listTeamSources is the team-scoped twin (GET /api/teams/{id}/sources):
// the path team's catalog, for a surface that names its team.
export async function listTeamSources(teamID: string): Promise<Source[]> {
  const r = await guard404("sources", () =>
    apiRequest<{ sources?: Source[] }>(
      `/api/teams/${encodeURIComponent(teamID)}/sources`,
      {},
    ),
  );
  return r.sources ?? [];
}

// sourceIconURL is the icon route for an <img src>: the session cookie
// authenticates it and the answer is always an image — the sanitized mark
// or the monogram — so the component carries no fallback of its own.
export function sourceIconURL(connectorID: string): string {
  // Through the pane's scope, as apiRequest routes every call: a desktop
  // workspace pane serves its own backend under a prefix.
  return `${scopePrefix()}/api/v1/connectors/${encodeURIComponent(connectorID)}/icon`;
}

// Labels of the closed category taxonomy (pkg/connector/spec Category).
// An unknown category renders raw, so a later taxonomy level stays visible.
export const SOURCE_CATEGORY_LABELS: Record<string, string> = {
  source_control: "Forge & dépôts",
  chat: "Chat",
  issue_tracker: "Suivi de projets",
  incident: "Incidents",
  monitoring: "Supervision",
  feeds: "Flux",
};

export function sourceCategoryLabel(category?: string): string {
  if (!category) return "Autres";
  return SOURCE_CATEGORY_LABELS[category] ?? category;
}

// Delivery labels: "webhook" when the vendor pushes, "interrogé" when
// iterion polls. An unknown value renders raw.
export const SOURCE_DELIVERY_LABELS: Record<string, string> = {
  push: "webhook",
  poll: "interrogé",
  mixed: "webhook + interrogé",
};

export function sourceDeliveryLabel(delivery?: string): string {
  if (!delivery) return "";
  return SOURCE_DELIVERY_LABELS[delivery] ?? delivery;
}

// formatPollInterval words a package's Go duration ("5m", "1h30m") in
// French units; a value outside that grammar renders raw.
export function formatPollInterval(interval: string): string {
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(interval.trim());
  if (!m || interval.trim() === "") return interval;
  const parts: string[] = [];
  if (m[1]) parts.push(`${m[1]} h`);
  if (m[2]) parts.push(`${m[2]} min`);
  if (m[3]) parts.push(`${m[3]} s`);
  return parts.join(" ");
}

// pollContractText is what polling promises (ADR-168 §7), in the studio's
// words: the server's `contract` says the same in English.
export function pollContractText(interval?: string): string {
  const cadence = interval
    ? `Interrogé toutes les ${formatPollInterval(interval)}`
    : "Interrogé périodiquement";
  return (
    `${cadence} : voit les versions qu'il observe à chaque lecture, pas l'historique ` +
    "entre deux lectures. L'activer ne rejoue pas l'existant ; un backfill demande " +
    "explicitement les derniers éléments."
  );
}
