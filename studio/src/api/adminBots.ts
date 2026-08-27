// Platform bot overrides — REST client for /api/admin/bots (super-admin).
// The DB-backed form of the baked bot catalog: a pushed bundle overrides
// the same-slug baked bot for every tenant from the next launch; deleting
// it reverts to the baked catalog. Mirrors admin_bots_routes.go.

import { request } from "./client";

export interface PlatformBotRow {
  id: string;
  slug: string;
  version: number;
  /** sha256 of the sorted file map — "what exactly is deployed". */
  digest?: string;
  origin?: string;
  created_by?: string;
  updated_by?: string;
  created_at: string;
  updated_at: string;
}

interface ListResponse {
  bot_sources: PlatformBotRow[];
}

export function listPlatformBots(): Promise<PlatformBotRow[]> {
  return request<ListResponse>("/admin/bots").then((r) => r.bot_sources ?? []);
}

export function deletePlatformBot(slug: string): Promise<void> {
  return request(`/admin/bots/${encodeURIComponent(slug)}`, {
    method: "DELETE",
  }).then(() => undefined);
}
