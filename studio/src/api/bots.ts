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
  /** Runtime repository need (manifest `repo:` block). Present when a bot
   *  declares it wants a repo target for the run; the Launch surfaces
   *  render a "Target repository" section (attach / create-new /
   *  optional-skip) driven off this. `mode: "none"` behaves like an
   *  absent block (kept so a bot can document the choice). */
  repo?: RepoRequirement;
  /** Scoped config-share surface (manifest `config_share:` block) — which
   *  fields of the bot's config file a non-operator may edit through a share
   *  URL. Present only when the bot declares one; the "Share config" card
   *  reads it to drive a data-driven mint form and is hidden otherwise. */
  config_share?: ConfigShareSpec;
  /** Typed routing contract (manifest `invocations:`) — how this bot can be
   *  triggered (forge event, /slash-command, schedule, board) and the
   *  execution mode each path uses. Drives the Integrations picker grouping.
   *  Empty for orchestrators (Nexie/Evoly) and loose .bot files. */
  invocations?: Invocation[];
  /** Launch-form hints (manifest `launch:` block) — which vars the bot
   *  considers its main inputs vs noise. Purely presentational: the launch
   *  form regroups its buckets from this; requiredness/validation and the
   *  engine's var resolution are untouched. */
  launch?: BotLaunchHints;
  /** Conversational-bot declaration (manifest `chat:` block). Present ONLY
   *  on bots the studio hosts in the assistant dock; absent on every
   *  ordinary bot, which is what makes this list the chat registry. */
  chat?: BotChatSurface;
}

/** BotChatSurface mirrors the manifest `chat:` block (pkg/bundle/chat.go).
 *  It carries presentation shape only — which node speaks, which one takes
 *  the reply — never what the bot means. */
export interface BotChatSurface {
  /** Overrides for the picker; empty falls back to display_name/description. */
  label?: string;
  description?: string;
  /** Launch var carrying the operator's first message. */
  seed_var?: string;
  /** node id → how the transcript renders it. A node absent from the map
   *  renders as an ordinary run event rather than disappearing. */
  nodes?: Record<string, BotChatNode>;
  launcher_vars?: BotChatLauncherVar[];
  launcher?: BotChatLauncher;
}

export interface BotChatNode {
  kind: "banner" | "human" | "silent";
  label?: string;
  summary_field?: string;
  prompt?: string;
  text_field?: string;
  approved_field?: string;
}

export interface BotChatLauncherVar {
  name: string;
  label?: string;
  /** Studio-side pre-fill source. "work_dir" is the only one understood;
   *  anything else is ignored so a newer bundle degrades to an empty
   *  field rather than failing to render. */
  default_from?: string;
}

export interface BotChatLauncher {
  prompt?: string;
  description?: string;
  submit_label?: string;
  allow_other?: boolean;
  presets?: BotChatSeedPreset[];
}

export interface BotChatSeedPreset {
  value: string;
  label?: string;
  description?: string;
}

/** BotLaunchHints mirrors the manifest `launch:` block. Unknown var names
 *  are ignored silently on both lists. */
export interface BotLaunchHints {
  /** Var names forced into the launch form's always-visible primary
   *  bucket, in the order listed — ahead of the heuristic primaries
   *  (required vars without a default). A var hinted here keeps its
   *  declared default and stays optional. */
  primary?: string[];
  /** Var names removed from the launch form entirely (never rendered in
   *  any bucket). The engine still applies their declared defaults; a
   *  name also listed in `primary` stays hidden. */
  hidden?: string[];
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
  board?: InvocationBoard;
}

/** InvocationBoard mirrors the manifest board: block on a kind=board
 *  invocation — the card-event filter a one-click trigger subscribes to.
 *  Absent = the bot is a plain dispatcher target (nothing to subscribe). */
export interface InvocationBoard {
  /** Card-event kinds (e.g. "card.moved"). Empty = any. */
  on?: string[];
  /** Fire only when the card enters one of these states. */
  to_states?: string[];
  /** Fire only when the card carries ALL of these labels. */
  all_labels?: string[];
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

/** RepoRequirement mirrors the manifest `repo:` block — the bot's runtime
 *  repository need. The Launch surfaces render it as the "Target
 *  repository" section (attach an active/other connected repo, create a
 *  new one on a connected forge, or opt out when the mode is optional).
 *  `mode: "none"` is equivalent to omitting the block; the launch surfaces
 *  treat it as "no section". Cloud-only launch path — the server 400s a
 *  repo-targeted launch in local mode. */
export interface RepoRequirement {
  /** "required" (launch soft-blocks without a target), "optional" (section
   *  offered, skippable), or "none" (explicit repo-independence). */
  mode: "required" | "optional" | "none";
  /** When true, the section offers a "create a new repository" path
   *  alongside "attach an existing one". */
  allow_create?: boolean;
  /** One-line operator-facing explanation shown under the section title. */
  purpose?: string;
  /** Seeds a created repo's default branch name (empty = forge default). */
  default_branch?: string;
  /** Seeds a created repo's visibility (default "private"). */
  visibility?: "private" | "public";
}

/** ConfigShareSpec mirrors the manifest `config_share:` block — the bot's
 *  scoped config-share surface. The mint DERIVES a share's editable/visible
 *  paths + config file from this (expanding a `{category}` placeholder), so a
 *  share can never exceed what the bot committed to git and a second bot
 *  adopts the editor by adding this block alone. */
export interface ConfigShareSpec {
  /** Config file inside the target repo a share edits (e.g. "feed-watch.json"). */
  config_path: string;
  /** Dotted leaf paths a share may WRITE. A `{category}` placeholder is
   *  expanded per share (e.g. "categories.{category}.feeds"). No globs. */
  editable_paths: string[];
  /** Extra read-only dotted paths a share may READ back as context (same
   *  `{category}` expansion). The projection returns editable ∪ visible. */
  visible_paths?: string[];
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
  /** True for team-authored bots (editable in the cloud editor); false for the
   *  read-only baked catalog. Undefined on older servers → treated read-only. */
  editable?: boolean;
  /** "tenant" for a team-authored bot, "catalog" for a baked one. */
  origin?: "tenant" | "catalog";
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

// ---------------------------------------------------------------------------
// Builder: create + templates — mirrors pkg/botscaffold (Spec / Template)
// and pkg/server/bots_create.go.
// ---------------------------------------------------------------------------

/** BotCreateVar mirrors botscaffold.VarSpec — one workflow var the
 *  builder declares on the scaffolded bot. */
export interface BotCreateVar {
  name: string;
  type: string;
  default?: string;
  description?: string;
}

/** BotCreateSpec mirrors botscaffold.Spec — the studio builder's wire
 *  body for POST /api/v1/bots. */
export interface BotCreateSpec {
  slug: string;
  display_name?: string;
  icon?: string;
  description?: string;
  when_to_use?: string;
  instructions: string;
  model?: string;
  backend?: string;
  skills?: string[];
  capabilities?: string[];
  vars?: BotCreateVar[];
  worktree?: boolean;
  sandbox?: boolean;
  permission?: string;
  max_cost_usd?: number;
  max_duration?: string;
  schedule_cron?: string;
}

/** BotTemplate is one "start from a template" gallery entry
 *  (GET /api/v1/bots/templates). `spec` is a ready-to-edit BotCreateSpec. */
export interface BotTemplate {
  id: string;
  icon: string;
  name: string;
  description: string;
  spec: BotCreateSpec;
}

/** listBotTemplates fetches the builder's static template gallery. */
export async function listBotTemplates(): Promise<BotTemplate[]> {
  const r = await apiRequest<{ templates: BotTemplate[] }>(`${BASE}/templates`);
  return r.templates ?? [];
}

/** createBot scaffolds a new bot bundle into the workspace's bots/
 *  directory and returns the discovered entry. Local-mode only (403 in
 *  cloud mode); 409 when the slug already exists. */
export function createBot(spec: BotCreateSpec): Promise<BotEntryWithSchema> {
  return apiRequest<BotEntryWithSchema>(BASE, {
    method: "POST",
    body: JSON.stringify(spec),
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

// ---- workflow-script import (POST /api/v1/bots/import) ----

/** ImportScriptRequest converts a Claude-Code workflow script (.js —
 *  the `export const meta` + agent()/phase()/log() shape) into a draft
 *  .bot. Lossy by contract: the server never executes the JS (goja AST
 *  walk) and everything unmappable degrades into `## IMPORT` markers +
 *  report entries. */
export interface ImportScriptRequest {
  source: string;
  filename?: string;
  name?: string;
  dry_run?: boolean;
}

export interface ImportScriptReport {
  mapped?: string[];
  holes?: string[];
  placeholders?: string[];
  dropped?: string[];
}

export interface ImportScriptResult {
  workflow_name: string;
  path?: string;
  dry_run?: boolean;
  needs_attention: boolean;
  report: ImportScriptReport;
  bot_source: string;
}

export function importBotScript(req: ImportScriptRequest): Promise<ImportScriptResult> {
  return apiRequest<ImportScriptResult>(`${BASE}/import`, {
    method: "POST",
    body: JSON.stringify(req),
  });
}
