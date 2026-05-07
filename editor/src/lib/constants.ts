import type { NodeKind } from "@/api/types";
import type { LucideIcon } from "lucide-react";
import {
  Bot,
  Scale,
  GitBranch,
  User,
  Wrench,
  Sigma,
  CheckCircle2,
  XCircle,
  Play,
} from "lucide-react";

/** Unicode glyphs for text-only contexts (breadcrumbs, mermaid exports,
 *  dropdown labels). For canvas rendering prefer NODE_LUCIDE_ICONS which
 *  gives consistent SVG output across OSes. */
export const NODE_ICONS: Record<NodeKind, string> = {
  agent: "\u{1F916}",
  judge: "\u{2696}\u{FE0F}",
  router: "\u{1F504}",
  human: "\u{1F464}",
  tool: "\u{1F527}",
  compute: "\u{03A3}",
  done: "\u{2705}",
  fail: "\u{274C}",
  start: "\u{25B6}\u{FE0F}",
};

/** Lucide icon component for each node kind — used for canvas rendering. */
export const NODE_LUCIDE_ICONS: Record<NodeKind, LucideIcon> = {
  agent: Bot,
  judge: Scale,
  router: GitBranch,
  human: User,
  tool: Wrench,
  compute: Sigma,
  done: CheckCircle2,
  fail: XCircle,
  start: Play,
};

/** Tailwind-500 family for consistent saturation. Red is reserved for
 *  `fail` + diagnostic-error styling — no other semantic meaning maps to red. */
export const NODE_COLORS: Record<NodeKind, string> = {
  agent: "#3B82F6",   // blue-500
  judge: "#8B5CF6",   // violet-500
  router: "#F97316",  // orange-500 (distinct from amber-warning)
  human: "#14B8A6",   // teal-500 — was #E74C3C, moved off red to free that color for errors
  tool: "#71717A",    // zinc-500
  compute: "#0EA5E9", // sky-500
  done: "#22C55E",    // green-500
  fail: "#EF4444",    // red-500 — error semantics
  start: "#10B981",   // emerald-500
};

/** Default node dimensions for layout and edge handle computation */
export const NODE_WIDTH = 160;
export const NODE_HEIGHT = 80;
export const AUX_NODE_WIDTH = 120;
export const AUX_NODE_HEIGHT = 44;

/** Layer overlay colors — single source of truth */
export type LayerKind = "schemas" | "prompts" | "vars";

export const LAYER_COLORS: Record<LayerKind, string> = {
  schemas: "#A78BFA",
  prompts: "#2DD4BF",
  vars: "#FBBF24",
};

export const LAYER_ICONS: Record<LayerKind, string> = {
  schemas: "\u{1F4D0}",
  prompts: "\u{1F4DD}",
  vars: "\u{1F3F7}\u{FE0F}",
};

export const LAYER_LABELS: Record<LayerKind, string> = {
  schemas: "Schemas",
  prompts: "Prompts",
  vars: "Vars",
};

/** Selection styling — shared across node and edge components */
export const SELECTED_BORDER = "#60A5FA";
export const SELECTED_GLOW = "0 0 10px rgba(96, 165, 250, 0.6)";

/** Sub-node kind type and styling — shared across DetailSubNode, SubNodePalette, nodeDetailGraph */
export type DetailSubKind = "schema" | "prompt" | "var" | "edge" | "tool";

export const SUB_COLORS: Record<DetailSubKind, string> = {
  schema: "#A78BFA",
  prompt: "#2DD4BF",
  var: "#FBBF24",
  edge: "#60A5FA",
  tool: "#34D399",
};

export const SUB_ICONS: Record<DetailSubKind, string> = {
  schema: "\u{1F4D0}",
  prompt: "\u{1F4DD}",
  var: "\u{1F3F7}\u{FE0F}",
  edge: "\u{1F517}",
  tool: "\u{1F527}",
};

/** Timing constants (ms) */
export const DEBOUNCE_EDGE_RECOMPUTE_MS = 100;
export const DEBOUNCE_FIT_VIEW_MS = 150;
export const DEBOUNCE_LAYOUT_SETTLE_MS = 300;
export const TOAST_DURATION_CONNECTION_ERROR_MS = 2000;
export const TOAST_DURATION_DEFAULT_MS = 3000;
