import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { tokenBlocks } from "./_cssTokens";
import { PERSONA, THEMES } from "./_tokens";

// The contrast audit (contrast.test.ts) judges a hand-written mirror of
// app.css: a token retuned in app.css would leave the mirror stale and the
// audit green on a value nobody ships. This holds the mirror to the file —
// every audited hex (and the -soft alphas) is read back from the two theme
// blocks of app.css (_cssTokens.ts, which refuses any shape it cannot
// model: a declaration outside the three blocks, an !important, a nested
// theme block) and must equal the mirror's.

const blocks = tokenBlocks(readFileSync(new URL("../../app.css", import.meta.url), "utf8"));

describe("a11y / app.css declares its tokens where the mirror reads them", () => {
  it("has every --color- declaration in @theme and the two top-level theme blocks, none !important", () => {
    expect(blocks.problems).toEqual([]);
  });

  it("keeps the @theme defaults equal to the dark block (no data-theme attribute renders dark)", () => {
    const drift = Object.entries(blocks.dark)
      .filter(([k, v]) => blocks.theme[k] !== v)
      .map(([k, v]) => `${k}: @theme ${blocks.theme[k] ?? "<absent>"} ≠ dark ${v}`);
    expect(drift).toEqual([]);
  });
});

// `rgba(220, 38, 38, 0.18)` → "#dc2626" and 0.18, the shape the mirror holds.
function rgbaHex(v: string): string {
  const m = v.match(/rgba\((\d+),\s*(\d+),\s*(\d+)/);
  return m
    ? "#" + [m[1], m[2], m[3]].map((n) => Number(n).toString(16).padStart(2, "0")).join("")
    : v;
}
function alpha(v: string): number {
  const m = v.match(/rgba\([^)]*,\s*([\d.]+)\)/);
  return m ? Number(m[1]) : 1;
}

for (const theme of ["dark", "light"] as const) {
  describe(`a11y / token mirror equals app.css (${theme})`, () => {
    const t = THEMES[theme];
    const decl = blocks[theme];

    it("surfaces", () => {
      for (const s of [0, 1, 2, 3] as const) expect(decl[`surface-${s}`]).toBe(t.surface[s]);
    });
    it("foreground tiers", () => {
      expect(decl["fg-default"]).toBe(t.fg.default);
      expect(decl["fg-muted"]).toBe(t.fg.muted);
      expect(decl["fg-subtle"]).toBe(t.fg.subtle);
      expect(decl["fg-onAccent"]).toBe(t.onAccent);
    });
    it("accent, accent-text and the accent-soft alpha", () => {
      expect(decl["accent"]).toBe(t.accent);
      expect(decl["accent-text"]).toBe(t.accentText);
      expect(decl["focus-ring"]).toBe(t.focusRing);
      const soft = decl["accent-soft"] ?? "";
      expect(rgbaHex(soft), "accent-soft").toBe(t.accent);
      expect(alpha(soft), "accent-soft alpha").toBeCloseTo(t.accentSoftA, 5);
    });
    it("border-control", () => {
      expect(decl["border-control"]).toBe(t.borderControl);
    });
    it("severity plain and -solid, soft tints and fg", () => {
      for (const [name, sev] of Object.entries(t.severity)) {
        expect(decl[name], `${name}`).toBe(sev.plain);
        // A missing declaration reads as undefined and fails on the equality.
        if ("solid" in sev) expect(decl[`${name}-solid`], `${name}-solid`).toBe(sev.solid);
        else expect(decl[`${name}-solid`], `${name}-solid (the mirror declares none)`).toBeUndefined();
        const soft = decl[`${name}-soft`] ?? "";
        expect(rgbaHex(soft), `${name}-soft`).toBe(sev.soft);
        expect(alpha(soft), `${name}-soft alpha`).toBeCloseTo(sev.softA, 5);
        expect(decl[`${name}-fg`], `${name}-fg`).toBe(sev.fg);
      }
    });
    it("persona palette", () => {
      for (const [hue, hex] of Object.entries(PERSONA[theme])) {
        expect(decl[`persona-${hue}`], `persona-${hue}`).toBe(hex);
      }
    });
  });
}
