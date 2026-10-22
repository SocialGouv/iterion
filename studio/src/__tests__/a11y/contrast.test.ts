import { describe, expect, it } from "vitest";
import { PERSONA, THEMES } from "./_tokens";

// Deterministic WCAG contrast audit for the design-system colour tokens.
//
// Why not axe-core here: axe's `color-contrast` rule needs a real browser
// canvas to sample computed pixel colours; under jsdom getContext() is
// unimplemented and the rule silently no-ops (a green that proves nothing).
// So we compute the WCAG 2.1 relative-luminance ratio directly from the
// token hex values mirrored from app.css (the mirror lives in
// _tokens.ts and tokens-mirror.test.ts holds it equal to the file), for
// BOTH themes. This is the regression guard the design-system "Open items
// / contrast sweep" asked for. A token value changes in app.css AND in
// _tokens.ts, or the mirror guard says so.
//
// AA thresholds: 4.5:1 for normal text, 3:1 for large/UI. fg-subtle is a
// deliberately de-emphasised tertiary token, held to AA on surfaces 0-2 —
// small subtle labels live on chips, drawers and popovers too — and to
// the 3:1 bar on surface-3, the transient hover lift.

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

    // fg-subtle: AA wherever small subtle text is authored — panels (0/1)
    // and the chip / drawer / popover surface (2); the 3:1 bar on
    // surface-3, the momentary hover lift.
    for (const s of [0, 1, 2] as const) {
      it(`fg-subtle on surface-${s} ≥ ${AA_TEXT}`, () => {
        const ratio = contrast(hexToRgb(t.fg.subtle), hexToRgb(t.surface[s]));
        expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
      });
    }
    it(`fg-subtle on surface-3 ≥ ${AA_LARGE}`, () => {
      const ratio = contrast(hexToRgb(t.fg.subtle), hexToRgb(t.surface[3]));
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

    // Accent-TEXT (links / icons) on the surfaces accent text is written
    // on — the panels, the surface-2 rows and popovers, and the
    // accent-soft chips — the decoupled token that fixed the old blue-600
    // link contrast (~3.4:1). Must clear AA everywhere, which is the
    // constraint that keeps --color-accent-text bright enough (indigo-400
    // was 3.5:1 on surface-2 and 4.4:1 on an accent-soft chip in dark).
    for (const s of [0, 1, 2] as const) {
      it(`accent-text on surface-${s} ≥ ${AA_TEXT}`, () => {
        const ratio = contrast(hexToRgb(t.accentText), hexToRgb(t.surface[s]));
        expect(ratio).toBeGreaterThanOrEqual(AA_TEXT);
      });
    }
    it(`accent-text on accent-soft (over surface-1) ≥ ${AA_TEXT}`, () => {
      const bg = composite(hexToRgb(t.accent), t.accentSoftA, hexToRgb(t.surface[1]));
      expect(contrast(hexToRgb(t.accentText), bg)).toBeGreaterThanOrEqual(AA_TEXT);
    });

    // Severity, decoupled like accent / accent-text: the plain token is
    // the text / icon / border colour (text-danger, border-warning), the
    // -solid token the background under a white label (Button danger /
    // success, DiagnosticBadge). One token for both roles cannot hold:
    // the -600 greens and ambers held a white label at 3.2-3.3:1, and
    // the -700 that fixed the label read 2.9:1 as text on surface-1.
    // The plain colour clears AA on the panels (0/1), the 3:1 non-text
    // bar on surface-2 (dark red-400 / cyan-500 read 3.7 / 4.3 there —
    // the residual, for icons and borders) and, as an icon, 3:1 on its
    // own -soft tint (IconButton hover; text on a tint is the -fg).
    for (const name of ["danger", "warning", "success", "info"] as const) {
      const sev = t.severity[name];
      it(`fg-onAccent on ${name}-solid ≥ ${AA_TEXT}`, () => {
        expect(contrast(hexToRgb(t.onAccent), hexToRgb(sev.solid))).toBeGreaterThanOrEqual(AA_TEXT);
      });
      for (const s of [0, 1] as const) {
        it(`${name} (text) on surface-${s} ≥ ${AA_TEXT}`, () => {
          expect(contrast(hexToRgb(sev.plain), hexToRgb(t.surface[s]))).toBeGreaterThanOrEqual(AA_TEXT);
        });
      }
      it(`${name} (icon/border) on surface-2 ≥ ${AA_LARGE}`, () => {
        expect(contrast(hexToRgb(sev.plain), hexToRgb(t.surface[2]))).toBeGreaterThanOrEqual(AA_LARGE);
      });
      it(`${name} (icon) on ${name}-soft (over surface-1) ≥ ${AA_LARGE}`, () => {
        const bg = composite(hexToRgb(sev.soft), sev.softA, hexToRgb(t.surface[1]));
        expect(contrast(hexToRgb(sev.plain), bg)).toBeGreaterThanOrEqual(AA_LARGE);
      });
    }

    // Control boundary (WCAG 1.4.11 non-text contrast / RGAA 3.3): the
    // border of a text field, checkbox or radio must hold 3:1 against
    // every surface controls sit on — panels (0/1) and the surface-2
    // drawers and cards (settings, var editors, PR sections).
    for (const s of [0, 1, 2] as const) {
      it(`border-control on surface-${s} ≥ ${AA_LARGE}`, () => {
        const ratio = contrast(hexToRgb(t.borderControl), hexToRgb(t.surface[s]));
        expect(ratio).toBeGreaterThanOrEqual(AA_LARGE);
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

// Persona identity palette — see _tokens.ts for the mirror.

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
