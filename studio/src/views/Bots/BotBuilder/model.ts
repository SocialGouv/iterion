// Extracted from BotBuilder/index.tsx to keep that file focused.
// The builder's draft model (persisted to localStorage), template →
// draft seeding, the pure validation helpers, and the draft →
// BotCreateSpec mapping submitted to POST /api/v1/bots.

import type { BotCreateSpec, BotTemplate } from "@/api/bots";

import { isValidVarName } from "./slug";

export const DRAFT_KEY = "bot-builder-draft";

export const VAR_TYPES = ["string", "int", "bool", "float"] as const;
export type VarType = (typeof VAR_TYPES)[number];

export interface VarRow {
  name: string;
  type: VarType;
  default: string;
  description: string;
}

export interface BuilderDraft {
  phase: 1 | 2;
  templateId: string | null;
  name: string;
  icon: string;
  description: string;
  instructions: string;
  // Carried through from a template spec (not directly editable in the
  // four-field form) so template-provided routing metadata isn't lost.
  whenToUse: string;
  capabilities: string[];
  model: string;
  backend: string;
  skills: string[];
  vars: VarRow[];
  worktree: boolean;
  sandbox: boolean;
  permission: "off" | "ask" | "deny";
  maxCostUsd: string;
  maxDuration: string;
  scheduleCron: string;
}

// Setter shape shared by the form-section cards: shallow-merge a partial
// into the draft.
export type PatchDraft = (p: Partial<BuilderDraft>) => void;

export function emptyDraft(): BuilderDraft {
  return {
    phase: 1,
    templateId: null,
    name: "",
    icon: "",
    description: "",
    instructions: "",
    whenToUse: "",
    capabilities: [],
    model: "",
    backend: "",
    skills: [],
    vars: [],
    worktree: false,
    sandbox: false,
    permission: "off",
    maxCostUsd: "",
    maxDuration: "",
    scheduleCron: "",
  };
}

export function loadDraft(): BuilderDraft {
  try {
    const raw = localStorage.getItem(DRAFT_KEY);
    if (!raw) return emptyDraft();
    const parsed = JSON.parse(raw) as Partial<BuilderDraft>;
    // Merge over the empty draft so a stale/older draft shape can't
    // leave fields undefined.
    return { ...emptyDraft(), ...parsed };
  } catch {
    return emptyDraft();
  }
}

export function draftFromTemplate(t: BotTemplate): BuilderDraft {
  const s = t.spec;
  return {
    ...emptyDraft(),
    phase: 2,
    templateId: t.id,
    name: s.display_name || (t.id === "blank" ? "" : t.name),
    icon: s.icon || t.icon || "",
    description: s.description ?? "",
    instructions: s.instructions ?? "",
    whenToUse: s.when_to_use ?? "",
    capabilities: s.capabilities ?? [],
    model: s.model ?? "",
    backend: s.backend ?? "",
    skills: s.skills ?? [],
    vars: (s.vars ?? []).map((v) => ({
      name: v.name,
      type: (VAR_TYPES as readonly string[]).includes(v.type) ? (v.type as VarType) : "string",
      default: v.default ?? "",
      description: v.description ?? "",
    })),
    worktree: s.worktree ?? false,
    sandbox: s.sandbox ?? false,
    permission: s.permission === "ask" || s.permission === "deny" ? s.permission : "off",
    maxCostUsd: s.max_cost_usd != null ? String(s.max_cost_usd) : "",
    maxDuration: s.max_duration ?? "",
    scheduleCron: s.schedule_cron ?? "",
  };
}

// Rows that are entirely empty are ignored (dropped on submit); any
// partially-filled row must carry a valid, unique name.
export function activeVarRows(vars: VarRow[]): VarRow[] {
  return vars.filter((v) => v.name.trim() !== "" || v.default !== "" || v.description !== "");
}

export function varNamesValid(activeVars: VarRow[]): boolean {
  return (
    activeVars.every((v) => isValidVarName(v.name.trim())) &&
    new Set(activeVars.map((v) => v.name.trim())).size === activeVars.length
  );
}

export function costInputValid(maxCostUsd: string): boolean {
  return (
    maxCostUsd.trim() === "" || (Number.isFinite(Number(maxCostUsd)) && Number(maxCostUsd) > 0)
  );
}

export function buildCreateSpec(
  draft: BuilderDraft,
  slug: string,
  activeVars: VarRow[],
): BotCreateSpec {
  return {
    slug,
    display_name: draft.name.trim(),
    instructions: draft.instructions.trim(),
    ...(draft.icon ? { icon: draft.icon } : {}),
    ...(draft.description.trim() ? { description: draft.description.trim() } : {}),
    ...(draft.whenToUse.trim() ? { when_to_use: draft.whenToUse.trim() } : {}),
    ...(draft.model.trim() ? { model: draft.model.trim() } : {}),
    ...(draft.backend ? { backend: draft.backend } : {}),
    ...(draft.skills.length > 0 ? { skills: draft.skills } : {}),
    ...(draft.capabilities.length > 0 ? { capabilities: draft.capabilities } : {}),
    ...(activeVars.length > 0
      ? {
          vars: activeVars.map((v) => ({
            name: v.name.trim(),
            type: v.type,
            ...(v.default !== "" ? { default: v.default } : {}),
            ...(v.description.trim() ? { description: v.description.trim() } : {}),
          })),
        }
      : {}),
    ...(draft.worktree ? { worktree: true } : {}),
    ...(draft.sandbox ? { sandbox: true } : {}),
    ...(draft.permission !== "off" ? { permission: draft.permission } : {}),
    ...(draft.maxCostUsd.trim() !== "" ? { max_cost_usd: Number(draft.maxCostUsd) } : {}),
    ...(draft.maxDuration.trim() !== "" ? { max_duration: draft.maxDuration.trim() } : {}),
    ...(draft.scheduleCron.trim() !== "" ? { schedule_cron: draft.scheduleCron.trim() } : {}),
  };
}
