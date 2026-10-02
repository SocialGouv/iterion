// Team-scoped scheduled bots (cloud) — REST client for
// /api/teams/{id}/schedules. A schedule fires a bot on a cron against a
// repo (repo_url); provisioning (EnableRepoPanel / the orchestrator)
// creates them, this client lets the studio list and manage them.

import { guard404, request } from "./client";

export interface ScheduledBot {
  id: string;
  tenant_id: string;
  repo_integration_id?: string;
  bot_id: string;
  // 5-field standard cron, UTC.
  cron: string;
  vars?: Record<string, string>;
  repo_url?: string;
  repo_ref?: string;
  disabled?: boolean;
  next_fire_at: string;
  last_fire_at?: string;
  // Launch-time verdict of the last tick — pre-existing on the server
  // record but not previously mirrored in the studio type. Empty when
  // the tick succeeded; a message says why the launch itself was
  // refused (org-gate denial, run-service refusal).
  last_error?: string;
  last_error_at?: string;
  // Terminal outcome of the last RUN this schedule launched (#1426).
  // last_error above is the launch decision; these are the run result.
  // All empty until the first run reaches a terminal status.
  // last_run_error_code is a stable machine-readable classifier
  // (a store.FailureCode, e.g. "SANDBOX_DRIVER_UNAVAILABLE") so
  // dashboards can dispatch on it without parsing last_run_error.
  // Cleared on a successful run.
  last_run_id?: string;
  last_run_status?: string;
  last_run_error?: string;
  last_run_error_code?: string;
  last_run_at?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
  // Overlap policy + guard (pkg/schedgate). overlap defaults to "skip":
  // a tick that would overlap a still-live run of the same schedule
  // passes, audited (actions schedule.tick.* on the team audit trail).
  overlap?: "skip" | "allow" | "keepalive";
  max_concurrent?: number;
  guard?: string;
  guard_timeout?: string;
  guard_var?: string;
  // Always-on (keepalive): relaunch every interval_seconds instead of on
  // cron (mutually exclusive with cron). stale_after is the silence cutoff
  // after which a running run is treated dead and relaunched.
  interval_seconds?: number;
  stale_after?: string;
}

export async function listTeamSchedules(teamID: string): Promise<ScheduledBot[]> {
  // 404 → FeatureUnavailableError: a local-mode server has no cloudsched
  // store, so the route isn't registered at all. Callers render the
  // "not enabled" empty state instead of an error banner.
  const res = await guard404("schedules", () =>
    request<{ schedules: ScheduledBot[] }>(
      `/teams/${encodeURIComponent(teamID)}/schedules`,
    ),
  );
  return res.schedules ?? [];
}

// ScheduleCreateInput mirrors pkg/server createScheduleReq. The server
// validates cron (5-field UTC) and the schedgate policy, and returns the
// created row (201) with its computed next_fire_at.
export interface ScheduleCreateInput {
  bot_id: string;
  // Exactly one of cron / interval_seconds. interval_seconds makes it an
  // always-on (keepalive) schedule.
  cron?: string;
  interval_seconds?: number;
  vars?: Record<string, string>;
  repo_url?: string;
  repo_ref?: string;
  disabled?: boolean;
  overlap?: "skip" | "allow" | "keepalive";
  max_concurrent?: number;
  guard?: string;
  guard_timeout?: string;
  guard_var?: string;
  stale_after?: string;
}

export function createTeamSchedule(
  teamID: string,
  input: ScheduleCreateInput,
): Promise<ScheduledBot> {
  return request<ScheduledBot>(`/teams/${encodeURIComponent(teamID)}/schedules`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export interface SchedulePatch {
  cron?: string;
  interval_seconds?: number;
  vars?: Record<string, string>;
  repo_url?: string;
  repo_ref?: string;
  disabled?: boolean;
  // Policy fields patch individually; the server validates the MERGED
  // row (400 on e.g. max_concurrent without overlap=allow).
  overlap?: "skip" | "allow" | "keepalive";
  max_concurrent?: number;
  guard?: string;
  guard_timeout?: string;
  guard_var?: string;
  stale_after?: string;
}

export function updateTeamSchedule(
  teamID: string,
  scheduleID: string,
  patch: SchedulePatch,
): Promise<ScheduledBot> {
  return request<ScheduledBot>(
    `/teams/${encodeURIComponent(teamID)}/schedules/${encodeURIComponent(scheduleID)}`,
    { method: "PATCH", body: JSON.stringify(patch) },
  );
}

export function deleteTeamSchedule(
  teamID: string,
  scheduleID: string,
): Promise<void> {
  return request<void>(
    `/teams/${encodeURIComponent(teamID)}/schedules/${encodeURIComponent(scheduleID)}`,
    { method: "DELETE" },
  );
}
