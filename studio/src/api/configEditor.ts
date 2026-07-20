// Config-editor client — operator-facing, COOKIE-session-authed (uses the
// shared apiRequest so the normal session cookie, silent refresh, and global
// 401 handling all apply). Talks to the config-editor routes under
// /api/teams/{id}/config-editor/*.
//
// This is the network layer for the least-privilege `config_editor` role's
// studio shell (@/views/ConfigEditorShell). A config_editor is a real team
// member whose session lives in the same HttpOnly cookie as any operator, so
// these calls MUST ride the shared client (credentials: "include").
//
// It is deliberately DISTINCT from @/api/configShare — that module is the
// isolated, iws_-token / credentials:"omit" client for the anonymous,
// shell-less /config/:id editor, and is eslint-forbidden outside
// src/views/ConfigShare/**. Do not confuse the two: this file is the
// signed-in path, that one is the tokened public path.

import { apiRequest, guard404, ApiError, FeatureUnavailableError } from "./client";

export { FeatureUnavailableError };

// EditorShare is one config-share the signed-in config_editor may edit,
// as listed by GET /api/teams/{id}/config-editor/shares.
export interface EditorShare {
  id: string;
  bot_id: string;
  label: string;
  category: string;
  config_path: string;
  read_only: boolean;
  /** Bot-declared branding for the shell heading (manifest config_share). */
  editor_title?: string;
  editor_description?: string;
}

// EditorConfigResponse mirrors GET .../shares/{sid}/config. `config` is the
// file projected to the editable-field set only; `sha` is the
// optimistic-concurrency token that must be echoed back on PATCH.
export interface EditorConfigResponse {
  config: Record<string, unknown>;
  sha: string;
  bot_id: string;
  label: string;
  category: string;
  config_path: string;
  read_only: boolean;
}

// EditorPatchInput is the PATCH body: the base sha the edit was made against
// plus a sparse nested-object patch carrying ONLY the changed leaves.
export interface EditorPatchInput {
  sha: string;
  patch: Record<string, unknown>;
}

// EditorPatchResult is the discriminated outcome of a PATCH. Modeled on the
// proven ConfigShare result shape:
//   - "ok"          — 200; carries the new sha + list of changed dotted paths.
//   - "conflict"    — 409; the file changed under us. Carries the FRESH server
//                     projection + sha so the caller can render "yours vs
//                     theirs" and require an explicit overwrite. NEVER auto-retry.
//   - "not_editable"— 400; a field in the patch is off the editable list.
export type EditorPatchResult =
  | { kind: "ok"; sha: string; changed: string[] }
  | { kind: "conflict"; config: Record<string, unknown>; sha: string }
  | { kind: "not_editable"; message: string };

function teamBase(teamID: string): string {
  return `/api/teams/${encodeURIComponent(teamID)}/config-editor`;
}

// listEditorShares returns the config-shares the active team exposes to its
// config_editor members. 404 (feature disabled on this server) surfaces as a
// typed FeatureUnavailableError.
export function listEditorShares(teamID: string): Promise<EditorShare[]> {
  return guard404("config-editor", async () => {
    const r = await apiRequest<{ shares: EditorShare[] }>(`${teamBase(teamID)}/shares`);
    return r.shares ?? [];
  });
}

// getEditorConfig loads the editable projection of one share plus its sha.
export function getEditorConfig(
  teamID: string,
  shareID: string,
): Promise<EditorConfigResponse> {
  return guard404("config-editor", () =>
    apiRequest<EditorConfigResponse>(
      `${teamBase(teamID)}/shares/${encodeURIComponent(shareID)}/config`,
    ),
  );
}

// patchEditorConfig submits {sha, patch} and normalises the three server
// outcomes into EditorPatchResult. On a 409 the shared apiRequest collapses
// the error body to its message, so we re-read the fresh projection via
// getEditorConfig to recover the current {config, sha} for the conflict view —
// one extra round-trip only on the rare conflict path, and it stays entirely
// on the shared (cookie + silent-refresh) client rather than a raw fetch.
export async function patchEditorConfig(
  teamID: string,
  shareID: string,
  input: EditorPatchInput,
): Promise<EditorPatchResult> {
  try {
    const r = await apiRequest<{ sha: string; changed?: string[] }>(
      `${teamBase(teamID)}/shares/${encodeURIComponent(shareID)}/config`,
      { method: "PATCH", body: JSON.stringify(input) },
    );
    return { kind: "ok", sha: r.sha, changed: r.changed ?? [] };
  } catch (err) {
    if (err instanceof ApiError && err.status === 409) {
      const fresh = await getEditorConfig(teamID, shareID);
      return { kind: "conflict", config: fresh.config, sha: fresh.sha };
    }
    if (err instanceof ApiError && err.status === 400) {
      return { kind: "not_editable", message: err.message };
    }
    // 401 (session expiry) is handled inside apiRequest (silent refresh, else
    // global sign-out); anything else is a genuine failure — propagate it.
    throw err;
  }
}

// ---------------------------------------------------------------------------
// Cadence — the recurrence of the share's category lives in iterion's schedule
// store (visible in the Schedules UI), NOT the repo config. A config_editor may
// read/adjust ONLY the cron of the schedule bound to its (bot, category).
// ---------------------------------------------------------------------------

// EditorSchedule mirrors GET .../shares/{sid}/schedule. `exists` is false when
// the category has no schedule yet (creating one stays an operator action).
export interface EditorSchedule {
  exists: boolean;
  schedule_id?: string;
  cron?: string;
  disabled?: boolean;
  next_fire_at?: string;
}

// getEditorSchedule loads the cadence of the share's category schedule. A 404
// (schedules unavailable — local mode) surfaces as FeatureUnavailableError;
// callers treat that as "no cadence card".
export function getEditorSchedule(teamID: string, shareID: string): Promise<EditorSchedule> {
  return guard404("config-editor-schedule", () =>
    apiRequest<EditorSchedule>(
      `${teamBase(teamID)}/shares/${encodeURIComponent(shareID)}/schedule`,
    ),
  );
}

// patchEditorSchedule updates the cron of the share's category schedule. The
// server validates the cron and returns the new cron + next fire time. A 404
// means no schedule exists for this category (operator must create one first).
export function patchEditorSchedule(
  teamID: string,
  shareID: string,
  cron: string,
): Promise<{ cron: string; next_fire_at?: string }> {
  return apiRequest<{ cron: string; next_fire_at?: string }>(
    `${teamBase(teamID)}/shares/${encodeURIComponent(shareID)}/schedule`,
    { method: "PATCH", body: JSON.stringify({ cron }) },
  );
}

// EditorRun is one row of the REDUCED recent-runs view for the share's
// (bot, category): status + timestamps only — the server never returns run
// ids, inputs, or errors to the config_editor role.
export interface EditorRun {
  status: string;
  created_at: string;
  finished_at?: string;
}

// listEditorShareRuns loads the recent digests of the share's (bot, category)
// so the editor can see the effect of their edits (did it run, when, ok?). A
// 404 (runs unavailable) surfaces as FeatureUnavailableError; callers treat it
// as "no recent-digests panel".
export function listEditorShareRuns(teamID: string, shareID: string): Promise<EditorRun[]> {
  return guard404("config-editor-runs", async () => {
    const r = await apiRequest<{ runs: EditorRun[] }>(
      `${teamBase(teamID)}/shares/${encodeURIComponent(shareID)}/runs`,
    );
    return r.runs ?? [];
  });
}
