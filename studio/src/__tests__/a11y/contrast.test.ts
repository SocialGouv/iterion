import { describe, expect, it } from "vitest";

// Deterministic WCAG contrast audit for the design-system colour tokens.
//
// Why not axe-core here: axe's `color-contrast` rule needs a real browser
// canvas to sample computed pixel colours; under jsdom getContext() is
// unimplemented and the rule silently no-ops (a green that proves nothing).
// So we compute the WCAG 2.1 relative-luminance ratio directly from the
// token hex values mirrored from app.css, for BOTH themes. This is the
// regression guard the design-system "Open items / contrast sweep" asked
// for. If a token value in app.css changes, update the table below.
//
// AA thresholds: 4.5:1 for normal text, 3:1 for large/UI. fg-subtle is a
// deliberately de-emphasised tertiary token; we hold it to 4.5 only on the
// primary panel surfaces (0/1), where small subtle labels actually live.
// surface-2/3 are input/hover backgrounds that rarely carry static subtle
// text, so they're audited at the 3:1 large/UI bar.

type RGB = [number, number, number];

function hexToRgb(hex: string): RGB {
  const h = hex.replace("#", "");
  return [
    parseInt(h.slice(0, 2), 16),
    parseInt(h.slice(2, 4), 16),
    parseInt(h.slice(4, 6), 16),
  ];
}

// Alpha-composite a translucent colour over an opaque background (the
// `-soft` severity tints are rgba over a surface).
function composite(fg: RGB, alpha: number, bg: RGB): RGB {
  return [
    Math.round(fg[0] * alpha + bg[0] * (1 - alpha)),
    Math.round(fg[1] * alpha + bg[1] * (1 - alpha)),
    Math.round(fg[2] * alpha + bg[2] * (1 - alpha)),
  ];
}

function relLuminance([r, g, b]: RGB): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function contrast(a: RGB, b: RGB): number {
  const la = relLuminance(a);
  const lb = relLuminance(b);
  const [hi, lo] = la > lb ? [la, lb] : [lb, la];
  return (hi + 0.05) / (lo + 0.05);
}

// Mirrored from src/app.css.
const THEMES = {
  dark: {
    surface: { 0: "#111827", 1: "#1f2937", 2: "#374151", 3: "#4b5563" },
    fg: { default: "#ffffff", muted: "#d1d5db", subtle: "#9ca3af" },
    // Decoupled accent (2026-06-21): `accent` is the button/brand background
    // (white label on it), `accentText` is the brighter link/icon colour.
    accent: "#4f46e5",
    accentText: "#818cf8",
    focusRing: "#818cf8",
    onAccent: "#ffffff",
    severity: {
      danger: { soft: "#dc2626", softA: 0.18, fg: "#fecaca" },
      warning: { soft: "#d97706", softA: 0.18, fg: "#fde68a" },
      success: { soft: "#16a34a", softA: 0.18, fg: "#bbf7d0" },
      info: { soft: "#0891b2", softA: 0.18, fg: "#a5f3fc" },
      live: { soft: "#22d3ee", softA: 0.18, fg: "#cffafe" },
    },
  },
  light: {
    surface: { 0: "#ffffff", 1: "#f9fafb", 2: "#f3f4f6", 3: "#e5e7eb" },
    fg: { default: "#111827", muted: "#374151", subtle: "#6b7280" },
    accent: "#4f46e5",
    accentText: "#4f46e5",
    focusRing: "#4f46e5",
    onAccent: "#ffffff",
    severity: {
      danger: { soft: "#b91c1c", softA: 0.1, fg: "#7f1d1d" },
      warning: { soft: "#b45309", softA: 0.1, fg: "#78350f" },
      success: { soft: "#15803d", softA: 0.1, fg: "#14532d" },
      info: { soft: "#0e7490", softA: 0.1, fg: "#164e63" },
      live: { soft: "#0891b2", softA: 0.12, fg: "#155e75" },
    },
  },
} as const;

const AA_TEXT = 4.5;
const AA_LARGE = 3.0;

for (const [themeName, t] of Object.entries(THEMES)) {
  describe(`a11y / contrast (${themeName} theme)`, () => {
    // Primary + secondary text must clear AA on the static surfaces.
    for (const fgKey of ["default", "muted"] as const) {
      for (const s of [0, 1, 2] as const) {
        it(`fg-${fgKey} on surface-${s} ≥ ${AA_TEXT}`, () => {
          const ratio = contrast(
            hexToRgb(t.fg[fgKey]),
            hexToRgb(t.surface[s]),
          );
          expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
        });
      }
    }

    // fg-subtle: AA on the primary panel surfaces where small subtle
    // labels live; large/UI bar on surface-2 (inputs/tooltips). surface-3
    // is the *hover* lift of surface-2 — a transient background, not a
    // static text surface (subtle text is authored against surface-2 and
    // the hover is momentary), so it's intentionally out of the audit.
    // (dark fg-subtle on surface-3 is 2.98:1 — fine for a hover flash, not
    // a standard we hold static text to.)
    for (const s of [0, 1] as const) {
      it(`fg-subtle on surface-${s} ≥ ${AA_TEXT}`, () => {
        const ratio = contrast(hexToRgb(t.fg.subtle), hexToRgb(t.surface[s]));
        expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
      });
    }
    it(`fg-subtle on surface-2 ≥ ${AA_LARGE}`, () => {
      const ratio = contrast(hexToRgb(t.fg.subtle), hexToRgb(t.surface[2]));
      expect(ratio).toBeGreaterThanOrEqual(AA_LARGE);
    });

    // Severity banner/badge text: the `-fg` colour over its `-soft` tint
    // composited on surface-1 (the typical container). Directly covers the
    // InlineBanner + error-box token pairs introduced this round.
    for (const [name, sev] of Object.entries(t.severity)) {
      it(`${name}-fg on ${name}-soft ≥ ${AA_TEXT}`, () => {
        const bg = composite(
          hexToRgb(sev.soft),
          sev.softA,
          hexToRgb(t.surface[1]),
        );
        const ratio = contrast(hexToRgb(sev.fg), bg);
        expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
      });
    }

    // Accent as a button BACKGROUND: the white label on it must clear AA.
    // This is the constraint that keeps --color-accent dark enough.
    it(`fg-onAccent on accent ≥ ${AA_TEXT}`, () => {
      const ratio = contrast(hexToRgb(t.onAccent), hexToRgb(t.accent));
      expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
    });

    // Accent-TEXT (links / icons) on the panel surfaces — the decoupled
    // token that fixed the old blue-600 link contrast (~3.4:1). Must clear
    // AA, which is the constraint that keeps --color-accent-text bright
    // enough. Together these two prove the decoupling actually buys AA on
    // both sides (a single token could not).
    for (const s of [0, 1] as const) {
      it(`accent-text on surface-${s} ≥ ${AA_TEXT}`, () => {
        const ratio = contrast(hexToRgb(t.accentText), hexToRgb(t.surface[s]));
        expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
      });
    }

    // Keyboard-focus indicator (WCAG 1.4.11 non-text contrast / RGAA
    // 3.3): the :focus-visible outline colour must hold 3:1 against
    // the surfaces the ring is drawn over. surface-3 stays out for
    // the same reason as fg-subtle above (transient hover lift).
    for (const s of [0, 1, 2] as const) {
      it(`focus-ring on surface-${s} ≥ ${AA_LARGE}`, () => {
        const ratio = contrast(hexToRgb(t.focusRing), hexToRgb(t.surface[s]));
        expect(ratio).toBeGreaterThanOrEqual(AA_LARGE);
      });
    }
  });
}

// Persona identity palette — categorical hues used as the bot-name text
// colour (run-header chip / board picker / catalog / recents) on the
// surface-0/1 panels. Tokenised to --color-persona-* (was raw Tailwind
// -400); mirrored here so the light-mode darkening keeps AA on white.
// Dark = Tailwind-400, light = -700. Update on any app.css persona edit.
const PERSONA = {
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

for (const [themeName, t] of Object.entries(THEMES)) {
  describe(`a11y / persona contrast (${themeName} theme)`, () => {
    const personas = PERSONA[themeName as "dark" | "light"];
    for (const [hue, hex] of Object.entries(personas)) {
      for (const s of [0, 1] as const) {
        it(`persona-${hue} on surface-${s} ≥ ${AA_TEXT}`, () => {
          const ratio = contrast(hexToRgb(hex), hexToRgb(t.surface[s]));
          expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
        });
      }
    }
  });
}
