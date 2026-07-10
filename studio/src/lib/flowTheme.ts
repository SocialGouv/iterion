import type { CSSProperties } from "react";

// Theme wiring for React Flow's built-in chrome (Controls, MiniMap).
//
// xyflow's own light/dark palettes ride the `colorMode` prop (a `.dark`
// class on the flow container), which follows the theme *store* — not
// the `data-theme` attribute the rest of the studio themes from. Rather
// than trust the two to stay in sync, we pin the chrome directly to the
// app's design tokens via xyflow's documented override seam: every
// styled property resolves `var(--xy-X, var(--xy-X-default))`, so
// setting the non-`-default` custom property on the component wins in
// both themes and tracks `data-theme` switches instantly.

// Spread onto <Controls style={...}>. The buttons and their SVG glyphs
// (fill: currentColor) pick these up from the container.
export const FLOW_CONTROLS_STYLE = {
  "--xy-controls-button-background-color": "var(--color-surface-1)",
  "--xy-controls-button-background-color-hover": "var(--color-surface-2)",
  "--xy-controls-button-color": "var(--color-fg-muted)",
  "--xy-controls-button-color-hover": "var(--color-fg-default)",
  "--xy-controls-button-border-color": "var(--color-border-default)",
  "--xy-controls-box-shadow": "0 0 0 1px var(--color-border-default)",
} as CSSProperties;

// MiniMap surface + the mask drawn outside the viewport rectangle.
// Passed as component props (bgColor/maskColor) rather than CSS so the
// SVG attributes resolve the tokens at paint time.
export const FLOW_MINIMAP_BG = "var(--color-surface-1)";
export const FLOW_MINIMAP_MASK = "var(--color-scrim-soft)";

// Border for the minimap panel itself so it reads as a card on both
// themes instead of a naked rectangle floating on the canvas.
export const FLOW_MINIMAP_STYLE: CSSProperties = {
  border: "1px solid var(--color-border-default)",
  borderRadius: 6,
  overflow: "hidden",
};
