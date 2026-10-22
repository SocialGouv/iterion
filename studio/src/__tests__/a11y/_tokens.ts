// The hand-written mirror of the colour tokens in src/app.css, shared by
// the contrast audit (contrast.test.ts) and the mirror guard
// (tokens-mirror.test.ts) that holds it equal to the file. NOT a test.

// Mirrored from src/app.css.
export const THEMES = {
  dark: {
    surface: { 0: "#111827", 1: "#1f2937", 2: "#374151", 3: "#4b5563" },
    fg: { default: "#ffffff", muted: "#d1d5db", subtle: "#a6adb8" },
    // Decoupled accent (2026-06-21): `accent` is the button/brand background
    // (white label on it), `accentText` is the brighter link/icon colour.
    accent: "#4f46e5",
    accentText: "#a5b4fc",
    focusRing: "#818cf8",
    accentSoftA: 0.16,
    borderControl: "#868e9b",
    onAccent: "#ffffff",
    // Severity, decoupled the same way: `plain` is the text / icon /
    // border colour (text-danger), `solid` the background under a white
    // label (bg-danger-solid); `live` has no solid.
    severity: {
      danger: { plain: "#f87171", solid: "#dc2626", soft: "#dc2626", softA: 0.18, fg: "#fecaca" },
      warning: { plain: "#f59e0b", solid: "#b45309", soft: "#d97706", softA: 0.18, fg: "#fde68a" },
      success: { plain: "#22c55e", solid: "#15803d", soft: "#16a34a", softA: 0.18, fg: "#bbf7d0" },
      info: { plain: "#06b6d4", solid: "#0e7490", soft: "#0891b2", softA: 0.18, fg: "#a5f3fc" },
      live: { plain: "#22d3ee", soft: "#22d3ee", softA: 0.18, fg: "#cffafe" },
    },
  },
  light: {
    surface: { 0: "#ffffff", 1: "#f9fafb", 2: "#f3f4f6", 3: "#e5e7eb" },
    fg: { default: "#111827", muted: "#374151", subtle: "#667080" },
    accent: "#4f46e5",
    accentText: "#4f46e5",
    focusRing: "#4f46e5",
    accentSoftA: 0.12,
    borderControl: "#6b7280",
    onAccent: "#ffffff",
    // In light the -700 plain holds both roles, so solid = plain.
    severity: {
      danger: { plain: "#b91c1c", solid: "#b91c1c", soft: "#b91c1c", softA: 0.1, fg: "#7f1d1d" },
      warning: { plain: "#b45309", solid: "#b45309", soft: "#b45309", softA: 0.1, fg: "#78350f" },
      success: { plain: "#15803d", solid: "#15803d", soft: "#15803d", softA: 0.1, fg: "#14532d" },
      info: { plain: "#0e7490", solid: "#0e7490", soft: "#0e7490", softA: 0.1, fg: "#164e63" },
      live: { plain: "#0891b2", soft: "#0891b2", softA: 0.12, fg: "#155e75" },
    },
  },
} as const;

// Persona identity palette — categorical hues used as the bot-name text
// colour (run-header chip / board picker / catalog / recents) on the
// surface-0/1 panels. Tokenised to --color-persona-* (was raw Tailwind
// -400); mirrored here so the light-mode darkening keeps AA on white.
// Dark = Tailwind-400, light = -700. Update on any app.css persona edit.
export const PERSONA = {
  dark: {
    sky: "#38bdf8", emerald: "#34d399", violet: "#a78bfa", teal: "#2dd4bf",
    amber: "#fbbf24", cyan: "#22d3ee", rose: "#fb7185", orange: "#fb923c",
    lime: "#a3e635",
  },
  light: {
    sky: "#0369a1", emerald: "#047857", violet: "#6d28d9", teal: "#0f766e",
    amber: "#b45309", cyan: "#0e7490", rose: "#be123c", orange: "#c2410c",
    lime: "#4d7c0f",
  },
} as const;
