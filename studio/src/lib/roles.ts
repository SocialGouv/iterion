// The role vocabularies the identity surfaces share, and how to show them.
//
// They live here rather than next to one of their readers because the team
// page, the org page and the super-admin user drawer all render the same
// ladders: a second copy drifts the day a role is added on one side only.

import type { OrgRole, Role } from "@/api/auth";

// config_editor is first so it reads as the least-privileged entry: the
// demotion heuristic below compares indexes, and config_editor ranks
// outside the ladder server-side (ADR-078) — a change TO it is always a
// narrowing, whatever the previous role was.
export const TEAM_ROLES: readonly Role[] = [
  "config_editor",
  "viewer",
  "member",
  "admin",
  "owner",
] as const;

export const ORG_ROLES: readonly OrgRole[] = ["member", "admin", "owner"] as const;

// roleLabel gives the technical `config_editor` string a friendly display
// name; every other role renders as-is (matching the existing lowercase UI).
export function roleLabel(role: string): string {
  return role === "config_editor" ? "Config editor" : role;
}

// isDemotion reports whether moving from `from` to `to` lowers access, for
// the confirm prompt. Unknown roles rank -1, so a change involving one is
// treated as a demotion — a prompt too many beats a silent lockout.
export function isDemotion(from: string, to: string): boolean {
  return TEAM_ROLES.indexOf(to as Role) < TEAM_ROLES.indexOf(from as Role);
}
