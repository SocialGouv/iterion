// Bot registry — REST client. Mirrors pkg/server/bots_routes.go.
// All paths are relative to the studio's same-origin server.

import { apiRequest } from "./client";
import type { PresetsBlock, VarsBlock } from "./types";

const BASE = "/api/v1/bots";

// ---------------------------------------------------------------------------
// Types — mirror pkg/botregistry JSON tags
// ---------------------------------------------------------------------------

/** BotEntry is the metadata-only shape returned by the registry list
 *  endpoint and embedded inside BotEntryWithSchema. */
export interface BotEntry {
  name: string;
  /** Bundle persona from manifest.yaml `display_name` (e.g. "Nexie",
   *  "Revi"). Empty for loose .bot files / un-personified bundles. The
   *  studio shows it as the lead label with `name` as a muted aside. */
  display_name?: string;
  description?: string;
  path: string;
  /** Path made workspace-relative by the server (slash form), when a
   *  workspace root is known. The studio opens `<rel_path>/main.bot`
   *  directly instead of reconstructing it from the absolute `path`. */
  rel_path?: string;
  triggers?: string[];
  capabilities?: string[];
  /** Orchestrator-facing "use when" guidance (manifest when_to_use) that
   *  Nexie reads to route a task. Editable in the Bot metadata panel. */
  when_to_use?: string;
  /** Resolved catalog visibility: manifest `enabled` default composed
   *  with the workspace overlay. `false` = hidden from Nexie + the board
   *  picker (but still listed in the Catalog manager to flip back on).
   *  Absent is treated as enabled. */
  enabled?: boolean;
  /** True when this entry is a bundle (manifest.yaml + main.bot) and thus
   *  has metadata that can be edited; loose .bot files are read-only. */
  is_bundle?: boolean;
  /** Manifest author/version, surfaced so the Bot metadata panel can
   *  pre-fill + edit them. */
  author?: string;
  version?: string;
  /** Optional emoji avatar from manifest.yaml `icon` (e.g. "🧭"). Takes
   *  precedence over the studio's built-in persona map; see
   *  `botVisual()` in lib/personas.ts. Editable in the Bot metadata
   *  panel. */
  icon?: string;
  /** The manifest `enabled` DEFAULT (pre-overlay). The Bot panel edits
   *  this; `enabled` is the resolved value the Catalog manager overlay
   *  controls. They differ when a workspace overlay is active. */
  manifest_enabled?: boolean;
  /** Forge-access requirements (manifest `forge:` block). Present only
   *  when the bot declares forge ambitions; the Integrations flow reads
   *  it to auto-provision the webhook + token binding, and the Bot panel
   *  renders it read-only so an operator sees what enabling the bot on a
   *  repo will set up. */
  forge?: ForgeRequirements;
  /** Typed routing contract (manifest `invocations:`) — how this bot can be
   *  triggered (forge event, /slash-command, schedule, board) and the
   *  execution mode each path uses. Drives the Integrations picker grouping.
   *  Empty for orchestrators (Nexie/Evoly) and loose .bot files. */
  invocations?: Invocation[];
}

/** Invocation mirrors the manifest `invocations:` entry. The payload field
 *  that applies is selected by `kind`. */
export interface Invocation {
  kind: "forge" | "command" | "schedule" | "board";
  mode?: "direct" | "board";
  args_var?: string;
  context_vars?: Record<string, string>;
  forge?: { event: string; actions?: string[] };
  command?: {
    name: string;
    aliases?: string[];
    scope?: string;
    min_replier_role?: string;
    disambiguator?: string;
  };
  schedule?: { suggested_cron?: string; default_vars?: Record<string, string> };
}

/** ForgeRequirements mirrors the manifest `forge:` block — what a bot
 *  needs to be auto-provisioned onto a connected repo. Advisory +
 *  discovery-time; the runtime does not read it. */
export interface ForgeRequirements {
  /** Normalized events the bot wants the webhook to subscribe to
   *  (`pull_request`, `pull_request_comment`). */
  events?: string[];
  /** Normalized permission map (key -> "read" | "write" | "admin");
   *  keys ∈ {pull_requests, repository, issues, webhooks}. */
  token_scopes?: Record<string, string>;
  /** Workflow-secret name the bot binds its forge token under
   *  (default "forge_token"). */
  secret?: string;
  webhook?: ForgeWebhookHints;
  /** Free text shown in the enable dialog explaining why the scopes are
   *  requested. */
  rationale?: string;
}

export interface ForgeWebhookHints {
  launch_vars?: Record<string, string>;
  min_replier_role?: string;
}

/** BotPatch is the editable subset of a bot's manifest. Omitted fields
 *  are left untouched server-side; an empty string clears a field. */
export type BotPatch = Partial<{
  display_name: string;
  description: string;
  author: string;
  version: string;
  when_to_use: string;
  enabled: boolean;
  triggers: string[];
  icon: string;
}>;

/** BotEntryWithSchema augments BotEntry with the workflow's declared
 *  vars + presets — same JSON shape as the studio's existing
 *  VarsBlock/PresetsBlock so VarFieldInput consumes it unchanged. */
export interface BotEntryWithSchema extends BotEntry {
  vars?: VarsBlock;
  presets?: PresetsBlock;
  /** Non-empty when the bot's source failed to parse. The picker still
   *  shows the bot but the typed form is hidden / surfaces an error. */
  schema_error?: string;
}

interface ListResponse {
  bots: BotEntryWithSchema[];
}

// ---------------------------------------------------------------------------
// REST surface
// ---------------------------------------------------------------------------

/** listBots returns every bot the host knows about along with its
 *  vars/presets schema. The full schemas are bundled in the list
 *  payload so the picker can switch bots without a second round trip. */
export async function listBots(): Promise<BotEntryWithSchema[]> {
  const r = await apiRequest<ListResponse>(BASE);
  return r.bots ?? [];
}

/** getBot fetches a single bot by name with its full schema. Useful
 *  when a ticket references a bot the list endpoint hasn't loaded
 *  yet (cache miss / page reload while a modal is open). */
export function getBot(name: string): Promise<BotEntryWithSchema> {
  return apiRequest<BotEntryWithSchema>(`${BASE}/${encodeURIComponent(name)}`);
}

/** updateBot writes a bot's manifest metadata (Bot metadata panel) and
 *  returns the refreshed entry. Bundle-only — the server rejects loose
 *  .bot files with 409. */
export function updateBot(name: string, patch: BotPatch): Promise<BotEntryWithSchema> {
  return apiRequest<BotEntryWithSchema>(`${BASE}/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(patch),
  });
}

/** setBotOverlay pins a bot's catalog visibility in this workspace
 *  without touching the (possibly git-tracked) manifest — the Catalog
 *  manager quick-toggle. `null` clears the override (manifest default
 *  stands again). Returns the refreshed entry. */
export function setBotOverlay(name: string, enabled: boolean | null): Promise<BotEntryWithSchema> {
  return apiRequest<BotEntryWithSchema>(`${BASE}/${encodeURIComponent(name)}/overlay`, {
    method: "PUT",
    body: JSON.stringify({ enabled }),
  });
}

export interface InstallBotRequest {
  url: string;
  ref?: string;
  path?: string;
  name?: string;
  force?: boolean;
}

export interface InstallBotResult {
  name: string;
  source: string;
  ref?: string;
  installed_path: string;
  skills: number;
  presets: number;
}

/** installBot imports a bot bundle from a git URL (or a local path on a
 *  self-hosted server) into the workspace, then returns where it landed.
 *  Local-mode only — the server returns 403 in cloud mode. */
export function installBot(req: InstallBotRequest): Promise<InstallBotResult> {
  return apiRequest<InstallBotResult>(`${BASE}/install`, {
    method: "POST",
    body: JSON.stringify(req),
  });
}

/** uploadBotBundle imports a `.botz` archive into the workspace by
 *  POSTing it as multipart/form-data to /api/v1/bots/upload. Uses a raw
 *  fetch (not apiRequest) so the browser sets the multipart boundary
 *  Content-Type itself. Local-mode only. `force` overwrites an existing
 *  install (the "update" path); `name` overrides the manifest name. */
export async function uploadBotBundle(
  file: File,
  opts: { force?: boolean; name?: string } = {},
): Promise<InstallBotResult> {
  const form = new FormData();
  form.append("file", file);
  if (opts.force) form.append("force", "true");
  if (opts.name) form.append("name", opts.name);
  const res = await fetch(`${BASE}/upload`, {
    method: "POST",
    credentials: "include",
    body: form,
  });
  if (!res.ok) {
    throw new Error(`upload failed (${res.status}): ${await res.text()}`);
  }
  return (await res.json()) as InstallBotResult;
}
