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

// isDemotion reports whether moving from `from` to `to` lowers access on a
// given ladder.
//
// A role the ladder does not carry is NOT comparable, and `indexOf`'s -1 is
// asymmetric — `-1 < n` is true but `n < -1` is false — so leaving it to the
// comparison would treat a move INTO an unknown role as a demotion and a move
// OUT of one as routine. `config_editor` is exactly that case: server-side it
// is orthogonal to viewer<member<admin<owner (rank 0, ADR-078), neither above
// nor below any of them, and moving off it revokes the only capability it
// grants. Both directions are treated as a demotion — a prompt too many beats
// a silent revocation.
export function isDemotion(
  from: string,
  to: string,
  ladder: readonly string[] = TEAM_ROLES,
): boolean {
  const f = ladder.indexOf(from);
  const t = ladder.indexOf(to);
  if (f < 0 || t < 0) return true;
  // config_editor sits in TEAM_ROLES so it can be rendered and selected, but
  // it is not a rung: any move across it changes WHICH capability is held,
  // not how much of one.
  if (from === "config_editor" || to === "config_editor") return true;
  return t < f;
}

// needsRoleChangeConfirm names the role edits worth a prompt: any demotion,
// and anything touching `owner` in either direction. Those are the two that
// lock someone out or hand over control; a routine promotion is not.
//
// It lives beside the ladders rather than in a page, because every surface
// that writes a membership role must apply the same rule — the super-admin
// drawer reaches ANY org or team on the platform, so it is the one that can
// least afford its own copy.
export function needsRoleChangeConfirm(
  from: string,
  to: string,
  ladder: readonly string[] = TEAM_ROLES,
): boolean {
  if (from === to) return false;
  return isDemotion(from, to, ladder) || from === "owner" || to === "owner";
}

// needsRoleGrantConfirm is the same rule for a role about to be GRANTED,
// where there is no previous role to compare against. Only the `owner` half
// applies: nothing is being taken away, but installing an owner hands over
// control just as much as promoting one — and `handlePutTeamMember` accepts
// any valid role from a team admin, so the grant path can reach it without
// ever passing through the change path that prompts.
export function needsRoleGrantConfirm(to: string): boolean {
  return to === "owner";
}

// confirmOwnerGrant asks before a grant that hands over control, and answers
// true when the caller may proceed.
//
// It takes the CONFIRMER rather than living in a component because the guard
// belongs to the WRITE: wired to a role `<select>` it guarded a gesture that
// writes nothing, and — since a grant form deliberately keeps its role for
// the next person — the second owner grant fired no prompt at all. Call it
// immediately before the request, from whatever surface makes it.
export async function confirmOwnerGrant(
  confirm: ((o: {
    title: string;
    message: string;
    confirmLabel?: string;
    confirmVariant?: "default" | "danger";
  }) => Promise<boolean>) | undefined,
  role: string,
): Promise<boolean> {
  if (!confirm || !needsRoleGrantConfirm(role)) return true;
  return confirm({
    title: "Grant ownership?",
    message: `"${roleLabel(role)}" hands over control of this tenant. Grant it?`,
    confirmLabel: "Grant owner",
    confirmVariant: "danger",
  });
}
