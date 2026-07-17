// Team-scoped scheduled bots (cloud) — REST client for
// /api/teams/{id}/schedules. A schedule fires a bot on a cron against a
// repo (repo_url); provisioning (EnableRepoPanel / the orchestrator)
// creates them, this client lets the studio list and manage them.

import { request } from "./client";

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
  created_by?: string;
  created_at: string;
  updated_at: string;
  // Overlap policy + guard (pkg/schedgate). overlap defaults to "skip":
  // a tick that would overlap a still-live run of the same schedule
  // passes, audited (actions schedule.tick.* on the team audit trail).
  overlap?: "skip" | "allow";
  max_concurrent?: number;
  guard?: string;
  guard_timeout?: string;
  guard_var?: string;
}

export async function listTeamSchedules(teamID: string): Promise<ScheduledBot[]> {
  const res = await request<{ schedules: ScheduledBot[] }>(
    `/teams/${encodeURIComponent(teamID)}/schedules`,
  );
  return res.schedules ?? [];
}

export interface SchedulePatch {
  cron?: string;
  vars?: Record<string, string>;
  repo_url?: string;
  repo_ref?: string;
  disabled?: boolean;
  // Policy fields patch individually; the server validates the MERGED
  // row (400 on e.g. max_concurrent without overlap=allow).
  overlap?: "skip" | "allow";
  max_concurrent?: number;
  guard?: string;
  guard_timeout?: string;
  guard_var?: string;
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
