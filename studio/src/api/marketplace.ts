// Hosted bot marketplace — REST client. Mirrors
// pkg/server/marketplace_routes.go and pkg/marketplace types.

import { apiRequest } from "./client";
import type { InstallBotResult } from "./bots";

const BASE = "/api/v1/marketplace";

// ---------------------------------------------------------------------------
// Types — mirror pkg/marketplace JSON tags
// ---------------------------------------------------------------------------

/** MarketplaceEntryPreset is the registry-facing slice of a bundle's
 *  preset metadata (no prompt body / vars map — install the bot for the
 *  full bias). */
export interface MarketplaceEntryPreset {
  name: string;
  display_name?: string;
  description?: string;
  skills?: string[];
}

/** MarketplaceScope mirrors marketplace.Scope — who may browse an entry
 *  once approved. Empty/absent reads as "public". */
export type MarketplaceScope = "public" | "instance" | "org";

/** MarketplaceStatus mirrors marketplace.Status. Empty/absent reads as
 *  "approved" (legacy + local single-tenant entries). */
export type MarketplaceStatus = "pending" | "approved" | "rejected";

/** MarketplaceEntry is one listing in the hosted bot registry. */
export interface MarketplaceEntry {
  slug: string;
  name: string;
  display_name?: string;
  description?: string;
  author?: string;
  tags?: string[];
  repo_url: string;
  ref?: string;
  subpath?: string;
  version?: string;
  readme?: string;
  presets?: MarketplaceEntryPreset[];
  installs: number;
  created_at?: string;
  updated_at?: string;
  // Multi-scope / moderation plumbing (cloud). Absent in local mode.
  scope?: MarketplaceScope;
  org_id?: string;
  status?: MarketplaceStatus;
  source?: "git" | "upload" | "builtin";
  submitted_by?: string;
  reviewed_by?: string;
  reviewed_at?: string;
  reject_reason?: string;
}

/** SubmitMarketplaceRequest is the wire body for
 *  POST /api/v1/marketplace/submit. */
export interface SubmitMarketplaceRequest {
  repo_url: string;
  ref?: string;
  path?: string;
  tags?: string[];
  /** Visibility scope (cloud only; ignored in local mode). */
  scope?: MarketplaceScope;
}

/** MarketplaceConfig mirrors GET /api/v1/marketplace/config — what the
 *  submit form needs to render the scope picker and whether submissions
 *  are moderated. */
export interface MarketplaceConfig {
  mode: "cloud" | "local";
  submit_enabled: boolean;
  scopes: MarketplaceScope[];
  default_scope: MarketplaceScope;
  moderated: boolean;
}

/** InstallMarketplaceResponse is the wire body returned by
 *  POST /api/v1/marketplace/bots/{slug}/install — the install result
 *  plus the post-bump entry (so callers don't need a follow-up GET). */
export interface InstallMarketplaceResponse {
  install: InstallBotResult;
  entry: MarketplaceEntry;
}

interface ListResponse {
  bots: MarketplaceEntry[];
}

// ---------------------------------------------------------------------------
// REST surface
// ---------------------------------------------------------------------------

/** listMarketplace returns every entry matching the optional free-text
 *  and tag filters. Returns [] for both omitted (server applies no
 *  filter). */
export async function listMarketplace(q?: string, tag?: string): Promise<MarketplaceEntry[]> {
  const params = new URLSearchParams();
  if (q && q.trim() !== "") params.set("q", q.trim());
  if (tag && tag.trim() !== "") params.set("tag", tag.trim());
  const suffix = params.toString() ? `?${params.toString()}` : "";
  const r = await apiRequest<ListResponse>(`${BASE}/bots${suffix}`);
  return r.bots ?? [];
}

/** getMarketplaceBot fetches a single entry by slug. */
export function getMarketplaceBot(slug: string): Promise<MarketplaceEntry> {
  return apiRequest<MarketplaceEntry>(`${BASE}/bots/${encodeURIComponent(slug)}`);
}

/** submitMarketplaceBot validates a repository and adds (or refreshes)
 *  the registry entry it publishes. Returns the persisted entry. */
export function submitMarketplaceBot(req: SubmitMarketplaceRequest): Promise<MarketplaceEntry> {
  return apiRequest<MarketplaceEntry>(`${BASE}/submit`, {
    method: "POST",
    body: JSON.stringify(req),
  });
}

/** installMarketplaceBot installs the entry's bundle into the workspace
 *  (.botz/) and returns the install result + the entry with its bumped
 *  install counter. Local-mode only (server returns 403 in cloud mode).
 *  Pass `force` to overwrite an existing install — the "update" path. */
export function installMarketplaceBot(
  slug: string,
  force = false,
): Promise<InstallMarketplaceResponse> {
  const suffix = force ? "?force=true" : "";
  return apiRequest<InstallMarketplaceResponse>(
    `${BASE}/bots/${encodeURIComponent(slug)}/install${suffix}`,
    { method: "POST" },
  );
}

/** uninstallMarketplaceBot removes the entry's bundle from the workspace
 *  (.botz/) and returns the (unchanged) entry so the caller can flip the
 *  card back to "Install". Local-mode only. */
export function uninstallMarketplaceBot(slug: string): Promise<MarketplaceEntry> {
  return apiRequest<MarketplaceEntry>(
    `${BASE}/bots/${encodeURIComponent(slug)}/install`,
    { method: "DELETE" },
  );
}

/** marketplaceDownloadUrl is the direct URL of a bot's packed `.botz`
 *  bundle. The endpoint is public (no auth — see isPublicMarketplaceRead),
 *  so this is meant to be used as an `<a href download>` target rather than
 *  fetched via apiRequest (the response is an archive, not JSON). */
export function marketplaceDownloadUrl(slug: string): string {
  return `${BASE}/bots/${encodeURIComponent(slug)}/download`;
}

/** getMarketplaceConfig returns the registry's submit configuration
 *  (allowed scopes, default, whether moderated). */
export function getMarketplaceConfig(): Promise<MarketplaceConfig> {
  return apiRequest<MarketplaceConfig>(`${BASE}/config`);
}

/** listModerationQueue returns the entries awaiting moderation, scoped to
 *  the caller's admin reach. Cloud + admin only (403/404 otherwise). */
export async function listModerationQueue(): Promise<MarketplaceEntry[]> {
  const r = await apiRequest<ListResponse>(`${BASE}/moderation`);
  return r.bots ?? [];
}

/** approveModeration approves a pending entry. */
export function approveModeration(slug: string): Promise<MarketplaceEntry> {
  return apiRequest<MarketplaceEntry>(
    `${BASE}/moderation/${encodeURIComponent(slug)}/approve`,
    { method: "POST" },
  );
}

/** rejectModeration rejects a pending entry with an optional reason. */
export function rejectModeration(slug: string, reason?: string): Promise<MarketplaceEntry> {
  return apiRequest<MarketplaceEntry>(
    `${BASE}/moderation/${encodeURIComponent(slug)}/reject`,
    { method: "POST", body: JSON.stringify({ reason: reason ?? "" }) },
  );
}
