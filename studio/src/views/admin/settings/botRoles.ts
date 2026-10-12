// Pure helpers for the Bot-roles console.
import type { BotRolesSettingsView } from "@/api/adminSettings";

export type BotRoleField = "reviewer" | "revi_converse" | "brancher" | "implementer";

export const BOT_ROLE_FIELDS: { field: BotRoleField; label: string; help: string }[] = [
  { field: "reviewer", label: "Reviewer", help: "PR/MR review" },
  { field: "revi_converse", label: "Revi converse", help: "conversational /revi answers" },
  { field: "brancher", label: "Brancher", help: "improves an existing branch" },
  { field: "implementer", label: "Implementer", help: "issue-labeled feature work" },
];

// storedRole reads one role's stored override (null when none / field absent).
export function storedRole(view: BotRolesSettingsView | undefined, field: BotRoleField): string | null {
  const rec = view?.stored;
  if (!rec) return null;
  const v = rec[field];
  return typeof v === "string" && v !== "" ? v : null;
}
