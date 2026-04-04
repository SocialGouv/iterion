import type { NodeKind } from "@/api/types";

/** Canonical icon for each node kind — single source of truth */
export const NODE_ICONS: Record<NodeKind, string> = {
  agent: "\u{1F916}",
  judge: "\u{2696}\u{FE0F}",
  router: "\u{1F504}",
  human: "\u{1F464}",
  tool: "\u{1F527}",
  done: "\u{2705}",
  fail: "\u{274C}",
  start: "\u{25B6}\u{FE0F}",
};

/** Canonical color for each node kind — single source of truth */
export const NODE_COLORS: Record<NodeKind, string> = {
  agent: "#4A90D9",
  judge: "#7B68EE",
  router: "#E67E22",
  human: "#E74C3C",
  tool: "#8B6914",
  done: "#2ECC71",
  fail: "#E74C3C",
  start: "#10B981",
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

/** Timing constants (ms) */
export const DEBOUNCE_EDGE_RECOMPUTE_MS = 100;
export const DEBOUNCE_FIT_VIEW_MS = 150;
export const DEBOUNCE_LAYOUT_SETTLE_MS = 300;
export const TOAST_DURATION_CONNECTION_ERROR_MS = 2000;
export const TOAST_DURATION_DEFAULT_MS = 3000;
