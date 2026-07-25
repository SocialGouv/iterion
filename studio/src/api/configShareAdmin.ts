// Config-share admin client — operator-facing, JWT-authed (uses the shared
// apiRequest so cookies, refresh, and 401 handling all apply). Talks to
// pkg/server/config_shares_routes.go under /api/teams/{id}/config-shares.
//
// This module is separate from the shell-less editor's client
// (@/api/configShare) precisely because the editor MUST NOT reach for a
// cookie-carrying fetch — see the comment atop that file.

import { apiRequest, guard404, FeatureUnavailableError } from "./client";

export { FeatureUnavailableError };

// ShareView mirrors pkg/server.Server.shareView — the plaintext token is
// NEVER in this shape. `ShareWithToken` returns it on create/rotate.
export interface ShareView {
  id: string;
  bot_id: string;
  label: string;
  repo_url: string;
  repo_ref: string;
  config_path: string;
  category: string;
  schema_ref: string;
  allowed_paths: string[];
  visible_paths: string[];
  read_only: boolean;
  enabled: boolean;
  token_last4: string;
  fingerprint: string;
  created_at: string;
  expires_at: string;
  revoked_at?: string;
  last_used_at?: string;
}

export interface ShareWithToken extends ShareView {
  token: string;
  url: string;
}

export interface CreateShareInput {
  bot_id: string;
  label: string;
  repo_url: string;
  repo_ref: string;
  /** Config file + paths are DERIVED server-side from the bot's manifest
   *  config_share: block when omitted (the normal path). Kept optional for a
   *  bot with no declared surface, where the operator supplies them. */
  config_path?: string;
  category: string;
  schema_ref?: string;
  allowed_paths?: string[];
  visible_paths?: string[];
  /** Optional least-privilege subset of the bot's declared editable fields
   *  (by leaf name, e.g. ["feeds"]) for a DERIVED grant. Omitted / full list =
   *  the whole declared surface. */
  editable_fields?: string[];
  read_only?: boolean;
  expires_days?: number;
  /** Opt out of the default TTL entirely — durable access (revoke via
   *  rotate/delete). Wins over expires_days. */
  never_expires?: boolean;
}

// Delivery mirrors configshare.Delivery — the forensic record of every
// editor-side request. Rendered as a table for the operator to audit.
export interface Delivery {
  id: string;
  share_id: string;
  tenant_id: string;
  at: string;
  source_ip: string;
  user_agent: string;
  method: string;
  /** "share:<id>" for a token edit, "user:<id>" for an authenticated
   *  config-editor session. Absent on legacy rows. */
  actor?: string;
  status: number;
  before_sha?: string;
  after_sha?: string;
  changed_paths?: string[];
  error?: string;
}

export function listConfigShares(teamID: string): Promise<ShareView[]> {
  return guard404("config-shares", async () => {
    const r = await apiRequest<{ shares: ShareView[] }>(
      `/api/teams/${encodeURIComponent(teamID)}/config-shares`,
    );
    return r.shares ?? [];
  });
}

export function createConfigShare(
  teamID: string,
  input: CreateShareInput,
): Promise<ShareWithToken> {
  return guard404("config-shares", () =>
    apiRequest<ShareWithToken>(`/api/teams/${encodeURIComponent(teamID)}/config-shares`, {
      method: "POST",
      body: JSON.stringify(input),
    }),
  );
}

export function rotateConfigShare(
  teamID: string,
  shareID: string,
): Promise<ShareWithToken> {
  return guard404("config-shares", () =>
    apiRequest<ShareWithToken>(
      `/api/teams/${encodeURIComponent(teamID)}/config-shares/${encodeURIComponent(shareID)}/rotate`,
      { method: "POST" },
    ),
  );
}

export function deleteConfigShare(teamID: string, shareID: string): Promise<void> {
  return guard404("config-shares", () =>
    apiRequest<void>(
      `/api/teams/${encodeURIComponent(teamID)}/config-shares/${encodeURIComponent(shareID)}`,
      { method: "DELETE" },
    ),
  );
}

export function listConfigShareDeliveries(
  teamID: string,
  shareID: string,
): Promise<Delivery[]> {
  return guard404("config-shares", async () => {
    const r = await apiRequest<{ deliveries: Delivery[] }>(
      `/api/teams/${encodeURIComponent(teamID)}/config-shares/${encodeURIComponent(shareID)}/deliveries`,
    );
    return r.deliveries ?? [];
  });
}
