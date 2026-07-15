import type { NodeKind } from "@/api/types";
import type { LucideIcon } from "lucide-react";
import {
  Bot,
  Scale,
  GitBranch,
  User,
  Wrench,
  Sigma,
  Boxes,
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
  subbot: "\u{1F9E9}",
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
  subbot: Boxes,
  done: CheckCircle2,
  fail: XCircle,
  start: Play,
};

/** Node-kind color tokens. Values are CSS `var(...)` references so the
 *  canonical hex lives in `app.css` (`--color-node-*`) and every
 *  consumer — inline styles, xyflow markerEnd, SVG fills — resolves
 *  the same value at paint time. Switching the palette is a one-line
 *  edit in app.css; no constants drift. */
export const NODE_COLORS: Record<NodeKind, string> = {
  agent: "var(--color-node-agent)",
  judge: "var(--color-node-judge)",
  router: "var(--color-node-router)",
  human: "var(--color-node-human)",
  tool: "var(--color-node-tool)",
  compute: "var(--color-node-compute)",
  subbot: "var(--color-node-subbot)",
  done: "var(--color-node-done)",
  fail: "var(--color-node-fail)",
  start: "var(--color-node-start)",
};

/** Blend a token color with the transparent layer. Replaces the legacy
 *  `${hex}1A` / `${hex}22` ergonomics — those only worked because the
 *  values were inline hex strings. With CSS vars we use color-mix.
 *  Default 13% (the historical "22" hex alpha ≈ 0.13). */
export function softColor(color: string, percent = 13): string {
  return `color-mix(in srgb, ${color} ${percent}%, transparent)`;
}

/** Default node dimensions for layout and edge handle computation */
export const NODE_WIDTH = 160;
export const NODE_HEIGHT = 80;
export const AUX_NODE_WIDTH = 120;
export const AUX_NODE_HEIGHT = 44;

/** Layer overlay colors — backed by `--color-layer-*` tokens. */
export type LayerKind = "schemas" | "prompts" | "vars";

export const LAYER_COLORS: Record<LayerKind, string> = {
  schemas: "var(--color-layer-schemas)",
  prompts: "var(--color-layer-prompts)",
  vars: "var(--color-layer-vars)",
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

/** Selection styling — shared across node and edge components.
 *  Backed by `--color-selected`. */
export const SELECTED_BORDER = "var(--color-selected)";
export const SELECTED_GLOW = `0 0 10px ${softColor("var(--color-selected)", 60)}`;

/** Sub-node kind type and styling — shared across DetailSubNode, SubNodePalette, nodeDetailGraph */
export type DetailSubKind = "schema" | "prompt" | "var" | "edge" | "tool";

export const SUB_COLORS: Record<DetailSubKind, string> = {
  schema: "var(--color-layer-schemas)",
  prompt: "var(--color-layer-prompts)",
  var: "var(--color-layer-vars)",
  edge: "var(--color-selected)",
  tool: "var(--color-sub-tool)",
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
