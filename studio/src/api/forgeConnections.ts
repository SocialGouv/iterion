import { guard404, request } from "./client";
import type { components } from "./schema";
import { apiGet, apiPost } from "./typed";

// Mirrors pkg/forge: the OUTBOUND forge-integration layer (connect a repo +
// auto-provision the inbound webhook + token binding). Distinct from
// api/webhooks.ts (the raw inbound-webhook CRUD).

// The forge enums the Go structs encode only as strings. We keep them as
// precise unions for provider-specific UI, then graft them onto the generated
// spec schemas below (Omit + &) so the FIELD SET stays spec-derived — a Go-side
// rename breaks the build — while these three fields stay narrowed.
export type ForgeProvider = "gitlab" | "github" | "forgejo";
export type ForgeKind = "oauth_app" | "github_app" | "pat";
export type ForgeConnectionStatus = "active" | "needs_reauth" | "revoked";

export type ForgeConnection = Omit<
  components["schemas"]["Connection"],
  "provider" | "kind" | "status"
> & {
  provider: ForgeProvider;
  kind: ForgeKind;
  status: ForgeConnectionStatus;
};

export type ForgeRepo = components["schemas"]["RepoSummary"];

export interface ForgeIntegration {
  id: string;
  connection_id: string;
  provider: ForgeProvider;
  repo_full_name: string;
  bot_ids: string[];
  events_normalized: string[];
  webhook_id: string;
  hook_id: string;
  hook_url?: string;
  managed_secret_id?: string;
  /** When true, the forge's issues are mirrored onto the team's native
   *  board (one-way forge→board sync). Toggled per-integration from the
   *  Integrations tab; absent on servers that predate the feature. */
  sync_issues_enabled?: boolean;
  created_at: string;
}

// ForgeTeamRepo is one row of the team-wide connected-repo aggregator
// (GET /api/teams/{id}/forge/repos) — the RepoSwitcher's data source: a
// RepoIntegration joined with its connection server-side.
export interface ForgeTeamRepo {
  connection_id: string;
  connection_status?: ForgeConnectionStatus | "degraded";
  provider: ForgeProvider;
  repo_full_name: string;
  clone_url?: string;
  web_url?: string;
  integration_id: string;
  bot_ids: string[];
  sync_issues_enabled: boolean;
}

/** Stable identity of a connected repo across connections. */
export function forgeTeamRepoKey(r: Pick<ForgeTeamRepo, "connection_id" | "repo_full_name">): string {
  return `${r.connection_id}::${r.repo_full_name}`;
}

export async function listTeamForgeRepos(teamID: string): Promise<ForgeTeamRepo[]> {
  const r = await guard404("forge_integrations", () =>
    request<{ repos: ForgeTeamRepo[] }>(`/teams/${teamID}/forge/repos`),
  );
  return r.repos ?? [];
}

// ForgeConnectionHealth is the connection's actionable live state — for a
// GitHub App it carries the installation's real repo scope and the GitHub
// settings URL where the scope/permissions can be widened.
export type ForgeConnectionHealth = components["schemas"]["forgeConnectionHealth"];

export async function getForgeConnectionHealth(
  teamID: string,
  connID: string,
): Promise<ForgeConnectionHealth> {
  return (await apiGet("/api/teams/{id}/forge/connections/{conn_id}/health", {
    params: { id: teamID, conn_id: connID },
  })) as ForgeConnectionHealth;
}

// createForgeRepo creates a NEW repository on a connected forge (the
// "new app → new repo" journey). Creation only — iterion never updates
// or deletes forge repositories.
export async function createForgeRepo(
  teamID: string,
  input: {
    connection_id: string;
    owner?: string;
    name: string;
    description?: string;
    private?: boolean;
    default_branch?: string;
    init_readme?: boolean;
  },
): Promise<{ repo: ForgeRepo; clone_url: string }> {
  return (await apiPost("/api/teams/{id}/forge/repos", {
    params: { id: teamID },
    body: input,
  })) as { repo: ForgeRepo; clone_url: string };
}

/** Counts returned by a manual issue-sync run (POST …/sync). */
export interface ForgeSyncResult {
  synced: number;
  created: number;
  updated: number;
}

export interface ForgeEnablePreview {
  events_normalized: string[];
  /** The forge's native event names the hook will subscribe to. */
  forge_native_events: string[];
  scopes: Record<string, string>;
  secrets: Array<{ bot_id: string; secret: string }>;
  /** Slash-commands the enabled bots add to the webhook (command → bot). */
  commands?: Array<{ command: string; bot_id: string }>;
  identity: { handle: string; provider: string; base_url: string };
  /** Non-empty = a bot can't be auto-installed (neither forge: nor an
   *  invocation). */
  conflicts: string[];
}

export interface ForgeProvisionResult {
  integration_id: string;
  webhook_id: string;
  hook_id: string;
  managed_secret_id: string;
  bot_ids: string[];
  created: boolean;
}

// ForgeOAuthApp is a per-tenant, per-instance OAuth application's credentials
// (client_id + sealed client_secret). The connect form offers OAuth for a
// (provider, instance) only when one of these exists for it. The field set is
// the spec's ForgeOAuthApp schema; provider stays a narrowed union. `installable`
// is true for a manifest-created GitHub App whose private key iterion holds (it
// can be INSTALLED — least-privilege github_app — not only OAuth-authorized).
export type ForgeOAuthApp = Omit<components["schemas"]["ForgeOAuthApp"], "provider"> & {
  provider: ForgeProvider;
};

export interface RegisterForgeOAuthAppInput {
  provider: ForgeProvider;
  forge_base_url?: string;
  /** "manual" pastes client_id+client_secret; "auto"/"auto_from_connection" call the forge API. */
  mode?: "manual" | "auto" | "auto_from_connection";
  client_id?: string;
  client_secret?: string;
  admin_token?: string;
  connection_id?: string;
}

export interface ConnectForgeInput {
  provider: ForgeProvider;
  mode: "oauth" | "pat" | "app";
  forge_base_url?: string;
  pat?: string;
  display_name?: string;
  /** Studio path to return to after an OAuth / App-install round-trip. */
  next?: string;
}

export interface ConnectForgeResult {
  connection?: ForgeConnection;
  /** Present for mode=oauth — the studio redirects the window here. */
  authorize_url?: string;
  /** Present for mode=app (GitHub) — the App install URL to redirect to. */
  install_url?: string;
}

export async function listForgeConnections(teamID: string): Promise<ForgeConnection[]> {
  const r = await guard404("forge_integrations", () =>
    apiGet("/api/teams/{id}/forge/connections", { params: { id: teamID } }),
  );
  return (r.connections ?? []) as ForgeConnection[];
}

export async function connectForge(
  teamID: string,
  input: ConnectForgeInput,
): Promise<ConnectForgeResult> {
  // Body type-checked against the spec's forgeConnectReq.
  return (await apiPost("/api/teams/{id}/forge/connections", {
    params: { id: teamID },
    body: input,
  })) as ConnectForgeResult;
}

export async function deleteForgeConnection(teamID: string, connID: string): Promise<void> {
  await request<void>(`/teams/${teamID}/forge/connections/${connID}`, { method: "DELETE" });
}

export async function listForgeRepos(
  teamID: string,
  connID: string,
  search?: string,
  page?: number,
): Promise<ForgeRepo[]> {
  const params = new URLSearchParams();
  if (search) params.set("search", search);
  if (page) params.set("page", String(page));
  const qs = params.toString() ? `?${params.toString()}` : "";
  const r = await request<{ repos: ForgeRepo[] }>(
    `/teams/${teamID}/forge/connections/${connID}/repos${qs}`,
  );
  return r.repos ?? [];
}

export async function listForgeIntegrations(teamID: string): Promise<ForgeIntegration[]> {
  const r = await guard404("forge_integrations", () =>
    request<{ integrations: ForgeIntegration[] }>(`/teams/${teamID}/forge/repo-bots`),
  );
  return r.integrations ?? [];
}

export async function previewForgeEnable(
  teamID: string,
  connID: string,
  repo: string,
  bots: string[],
): Promise<ForgeEnablePreview> {
  const params = new URLSearchParams({ connection_id: connID, repo, bots: bots.join(",") });
  return request(`/teams/${teamID}/forge/repo-bots/preview?${params.toString()}`);
}

export async function enableForgeRepoBots(
  teamID: string,
  connID: string,
  repo: string,
  botIDs: string[],
  scheduleCrons?: Record<string, string>,
): Promise<ForgeProvisionResult> {
  return request(`/teams/${teamID}/forge/repo-bots`, {
    method: "POST",
    body: JSON.stringify({
      connection_id: connID,
      repo,
      bot_ids: botIDs,
      schedule_crons:
        scheduleCrons && Object.keys(scheduleCrons).length > 0 ? scheduleCrons : undefined,
    }),
  });
}

// updateForgeRepoBots sets an integration's EXACT bot set (replace
// semantics — the per-bot unbind). Empty lists are rejected server-side;
// removing the last bot is disableForgeIntegration.
export async function updateForgeRepoBots(
  teamID: string,
  integrationID: string,
  botIDs: string[],
  scheduleCrons?: Record<string, string>,
): Promise<ForgeProvisionResult> {
  return request(`/teams/${teamID}/forge/repo-bots/${integrationID}`, {
    method: "PATCH",
    body: JSON.stringify({
      bot_ids: botIDs,
      schedule_crons:
        scheduleCrons && Object.keys(scheduleCrons).length > 0 ? scheduleCrons : undefined,
    }),
  });
}

export async function disableForgeIntegration(
  teamID: string,
  integrationID: string,
): Promise<void> {
  await request<void>(`/teams/${teamID}/forge/repo-bots/${integrationID}`, { method: "DELETE" });
}

// updateForgeIntegration patches a connected repo's integration — today
// just the issue-sync toggle. Returns the refreshed ForgeIntegration so the
// caller reflects the new `sync_issues_enabled` without a re-list.
export async function updateForgeIntegration(
  teamID: string,
  integrationID: string,
  patch: { sync_issues_enabled?: boolean },
): Promise<ForgeIntegration> {
  return request(`/teams/${teamID}/forge/integrations/${integrationID}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
}

// syncForgeIntegration triggers a one-shot forge→board issue sync and
// returns the counts (synced / created / updated) for the operator's toast.
export async function syncForgeIntegration(
  teamID: string,
  integrationID: string,
): Promise<ForgeSyncResult> {
  return request(`/teams/${teamID}/forge/integrations/${integrationID}/sync`, {
    method: "POST",
  });
}

// ForgeHook is a webhook registered on the forge side for an integration's
// repo. Mirrors the {id,url,events,active} shape the server normalizes from
// each provider's hook API — used to audit that the iterion webhook is still
// live on the forge.
export interface ForgeHook {
  id: string;
  url: string;
  events: string[];
  active: boolean;
}

// listForgeIntegrationHooks reads the forge-side registered hooks for an
// integration's repo, so the operator can audit them against what iterion
// provisioned.
export async function listForgeIntegrationHooks(
  teamID: string,
  integrationID: string,
): Promise<ForgeHook[]> {
  const r = await request<{ hooks: ForgeHook[] }>(
    `/teams/${teamID}/forge/integrations/${integrationID}/hooks`,
  );
  return r.hooks ?? [];
}

export async function listForgeOAuthApps(teamID: string): Promise<ForgeOAuthApp[]> {
  const r = await guard404("forge_integrations", () =>
    apiGet("/api/teams/{id}/forge/oauth-apps", { params: { id: teamID } }),
  );
  return (r.apps ?? []) as ForgeOAuthApp[];
}

export async function registerForgeOAuthApp(
  teamID: string,
  input: RegisterForgeOAuthAppInput,
): Promise<ForgeOAuthApp> {
  // Body type-checked against the spec's forgeOAuthAppReq.
  return (await apiPost("/api/teams/{id}/forge/oauth-apps", {
    params: { id: teamID },
    body: input,
  })) as ForgeOAuthApp;
}

export async function deleteForgeOAuthApp(teamID: string, appID: string): Promise<void> {
  await request<void>(`/teams/${teamID}/forge/oauth-apps/${appID}`, { method: "DELETE" });
}

export interface GitHubManifestStart {
  /** github.com (or GHE) URL to POST the manifest form to. */
  post_url: string;
  /** The GitHub App manifest to submit as a hidden `manifest` form field. */
  manifest: Record<string, unknown>;
  state: string;
}

// startGitHubManifest returns the pre-filled GitHub App manifest + the
// github.com POST target; the caller auto-submits a form so GitHub creates the
// App and redirects back to iterion's callback (which stores the credentials).
// getMyGitHubOrgs returns the caller's GitHub org logins (captured via the
// read:org picker below or at GitHub SSO login) — UI hints for the org dropdown.
export async function getMyGitHubOrgs(): Promise<string[]> {
  const r = await request<{ orgs: string[] }>("/me/github-orgs");
  return r.orgs ?? [];
}

// startGitHubOrgsPicker returns a GitHub authorize URL (read:org via the global
// OAuth app); the studio redirects there. On return the orgs are persisted, so
// getMyGitHubOrgs then populates the dropdown.
export async function startGitHubOrgsPicker(next: string): Promise<{ authorize_url: string }> {
  return request(`/forge/github/orgs/start?next=${encodeURIComponent(next)}`);
}

export async function startGitHubManifest(
  teamID: string,
  // github_org creates the App UNDER that org (installable org-wide); blank =
  // the caller's personal account (then only installable there).
  // allow_repo_creation requests administration:write on the App so iterion
  // can CREATE repositories (opt-in, surfaced as a visible checkbox).
  // allow_app_delivery requests workflows:write + packages:write so a bot can
  // publish the CI that builds an app and the image it produces — without
  // them GitHub refuses any push touching .github/workflows/**.
  input: {
    forge_base_url?: string;
    next?: string;
    github_org?: string;
    allow_repo_creation?: boolean;
    allow_app_delivery?: boolean;
  },
): Promise<GitHubManifestStart> {
  // Body type-checked against the spec's forgeOAuthAppReq (provider is
  // required there; this endpoint is GitHub-only, so state it explicitly).
  return (await apiPost("/api/teams/{id}/forge/oauth-apps/github-manifest", {
    params: { id: teamID },
    body: { provider: "github", ...input },
  })) as GitHubManifestStart;
}
