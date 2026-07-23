// Bot-sources client — team-authored bot bundles under
// /api/teams/{id}/bot-sources/*. This is the writable, tenant-scoped bot store
// that makes cloud bot editing possible: the studio editor persists here (via
// the botsource:// virtual path in client.ts openFile/saveFile), and the Bots
// gallery uses fork/create/delete through this module.
//
// Cookie-session-authed like the config-editor client — rides the shared
// apiRequest so refresh + 401 handling apply.

import { apiRequest, guard404 } from "./client";

export interface BotSourceMeta {
  id: string;
  slug: string;
  origin?: string;
  version: number;
  created_by?: string;
  updated_by?: string;
  created_at?: string;
  updated_at?: string;
}

export interface BotSourceFull extends BotSourceMeta {
  files: Record<string, string>;
}

function base(teamID: string): string {
  return `/api/teams/${encodeURIComponent(teamID)}/bot-sources`;
}

// listBotSources returns the team's authored bots (metadata only). 404 (feature
// off) surfaces as a typed FeatureUnavailableError.
export function listBotSources(teamID: string): Promise<BotSourceMeta[]> {
  return guard404("bot-sources", async () => {
    const r = await apiRequest<{ bot_sources: BotSourceMeta[] }>(base(teamID));
    return r.bot_sources ?? [];
  });
}

export function getBotSource(teamID: string, slug: string): Promise<BotSourceFull> {
  return apiRequest<BotSourceFull>(`${base(teamID)}/${encodeURIComponent(slug)}`);
}

// putBotSource creates or replaces a whole bundle. The server compiles it and
// returns 400 with diagnostics if it doesn't compile.
export function putBotSource(
  teamID: string,
  slug: string,
  files: Record<string, string>,
  version?: number,
): Promise<BotSourceFull> {
  return apiRequest<BotSourceFull>(`${base(teamID)}/${encodeURIComponent(slug)}`, {
    method: "PUT",
    body: JSON.stringify({ files, version }),
  });
}

// forkBotSource copies a read-only baked catalog bot into the team store under a
// new slug, so it becomes editable.
export function forkBotSource(
  teamID: string,
  slug: string,
  from: string,
): Promise<BotSourceFull> {
  return apiRequest<BotSourceFull>(`${base(teamID)}/${encodeURIComponent(slug)}/fork`, {
    method: "POST",
    body: JSON.stringify({ from }),
  });
}

export function deleteBotSource(teamID: string, slug: string): Promise<void> {
  return apiRequest<void>(`${base(teamID)}/${encodeURIComponent(slug)}`, {
    method: "DELETE",
  });
}

// putBotSourceFile writes one raw file into an existing bundle (skills/*.md,
// manifest.yaml, …). Distinct from the editor's document save path (which
// unparses a .bot document first) — this takes verbatim text. The server
// re-validates the whole bundle and returns 400 if it no longer compiles.
export function putBotSourceFile(
  teamID: string,
  slug: string,
  rel: string,
  content: string,
): Promise<BotSourceFull> {
  const path = rel.split("/").map(encodeURIComponent).join("/");
  return apiRequest<BotSourceFull>(
    `${base(teamID)}/${encodeURIComponent(slug)}/files/${path}`,
    { method: "PUT", body: JSON.stringify({ content }) },
  );
}

export function deleteBotSourceFile(
  teamID: string,
  slug: string,
  rel: string,
): Promise<BotSourceFull> {
  const path = rel.split("/").map(encodeURIComponent).join("/");
  return apiRequest<BotSourceFull>(
    `${base(teamID)}/${encodeURIComponent(slug)}/files/${path}`,
    { method: "DELETE" },
  );
}
