import { describe, expect, it } from "vitest";
import { files, scan } from "./_scanner";

// Regression guard for phantom / legacy Tailwind utility classes — names
// that have NO matching `--color-*` token in app.css, so Tailwind generates
// no rule and the element renders with no colour (an invisible button label,
// an unstyled primary button, a missing progress track…). Yesterday's UX
// round fixed 44 of these by hand; this gate keeps them — and later batches
// (bg-/text-/border-fg-accent / text-on-* / *-error / surface-overlay /
// text-fg-warn|success / text-text-* / bg-bg-* / bare border-border) — from
// creeping back.
//
// This bans only names known to be undefined. Adding a *real* token to
// app.css never requires touching this list. The canonical replacements:
//   bg-fg-accent        -> bg-accent
//   text-fg-accent      -> text-accent-text
//   text-on-accent      -> text-fg-onAccent
//   text-on-danger      -> text-danger-fg
//   text-error/bg-error -> text-danger / bg-danger-soft (severity token is "danger")
//   bg-surface-overlay  -> bg-black/N scrim (or surface-0..3)
//   text-fg-warn        -> text-warning-fg
//   text-fg-success     -> text-success-fg
//   text-fg-error       -> text-danger-fg
//   text-text-1/2       -> text-fg-default / text-fg-muted
//   bg-bg-*             -> bg-surface-*
//   border-border       -> border-border-default

const BANNED: { label: string; re: RegExp }[] = [
  { label: "(bg|text|border)-fg-accent — use bg-accent / text-accent-text (no --color-fg-accent token)", re: /\b(bg|text|border)-fg-accent\b/ },
  { label: "(text|bg)-on-* — use text-fg-onAccent / text-danger-fg (no --color-on-* token)", re: /\b(text|bg)-on-(accent|danger|success|warning|info)\b/ },
  { label: "(bg|text|border|ring)-error — severity token is 'danger' (text-danger, bg-danger-soft…)", re: /\b(bg|text|border|ring)-error\b/ },
  { label: "*-surface-overlay — no such token; use a bg-black/N scrim or surface-0..3", re: /\bsurface-overlay\b/ },
  { label: "text-fg-warn — use text-warning-fg", re: /\btext-fg-warn\b/ },
  { label: "text-fg-success — use text-success-fg", re: /\btext-fg-success\b/ },
  { label: "text-fg-error — use text-danger-fg", re: /\btext-fg-error\b/ },
  { label: "text-text-* — legacy double-prefix; use text-fg-*", re: /\btext-text-/ },
  { label: "bg-bg-* — phantom double-prefix; use bg-surface-*", re: /\bbg-bg-/ },
  { label: "bare border-border — use border-border-default", re: /\bborder-border(?![-\w])/ },
  { label: "*-accent-emphasis — no such token; use accent / accent-hover", re: /\baccent-emphasis\b/ },
  { label: "*-surface-default — no such token; use surface-0..3", re: /\bsurface-default\b/ },
  { label: "text-accent-contrast — no such token; use text-fg-onAccent", re: /\baccent-contrast\b/ },
  { label: "text-fg-link — no such token; use text-accent-text", re: /\btext-fg-link\b/ },
  { label: "text-content-N — no such token; use text-fg-muted / text-fg-subtle", re: /\btext-content-\d\b/ },
  { label: "bare text-fg — no such token; use text-fg-default", re: /(?<![\w-])text-fg(?![-\w])/ },
  { label: "bare border-default — no such token; use border-border-default", re: /(?<![\w-])border-default(?![-\w])/ },
];

describe("no phantom/legacy Tailwind classes", () => {
  it("scans a non-trivial number of source files", () => {
    expect(files.length).toBeGreaterThan(150);
  });

  for (const { label, re } of BANNED) {
    it(`bans ${label}`, () => {
      const hits = scan(re);
      expect(hits, `phantom class found — replace it:\n${hits.join("\n")}`).toEqual([]);
    });
  }
});

// ---------------------------------------------------------------------------
// Second guard: raw chromatic Tailwind palette utilities (text-amber-500,
// bg-red-300, …). These DO render — Tailwind keeps its default palette — but
// they bypass the semantic token system: a `-300` text on a light surface is
// near-invisible (no light-mode inversion), and the colour drifts away from
// the danger/warning/success/info severity language. Use the tokens:
//   text-amber-* / bg-amber-*  -> text-warning(-fg) / bg-warning(-soft|/N)
//   text-red-*   / bg-red-*    -> text-danger(-fg)  / bg-danger(-soft)
//   text-emerald-/green-*      -> text-success(-fg) / bg-success(-soft)
//   text-sky-/cyan-*           -> text-info(-fg)    / bg-info(-soft|/N)
// Non-chromatic neutrals (bg-black/N modal scrims, the bg-black video
// viewport, bg-white/N) carry no numeric palette step, so they are NOT
// matched and stay allowed.
const PALETTE_RE =
  /\b(text|bg|border|ring|fill|stroke|from|to|via|outline|decoration|divide|caret)-(amber|red|orange|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|slate|gray|zinc|neutral|stone)-\d/;

// Files allowed to use a categorical (non-semantic) hue palette. The bot
// persona palette was tokenised to --color-persona-* (app.css + personas.ts,
// contrast-audited), so this is currently empty — add a path here only for a
// genuinely categorical identity palette that cannot be a semantic token.
const PALETTE_ALLOW: string[] = [];

describe("no raw chromatic Tailwind palette (use semantic tokens)", () => {
  it("bans (text|bg|border|…)-<hue>-<step>", () => {
    const hits = scan(PALETTE_RE, (path) => PALETTE_ALLOW.some((a) => path.includes(a)));
    expect(
      hits,
      `raw palette colour found — use a semantic token:\n${hits.join("\n")}`,
    ).toEqual([]);
  });
});
