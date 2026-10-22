import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// Wiring guard for the keyboard-focus indicator (RGAA 10.7 / WCAG 2.4.7).
// contrast.test.ts holds the focus-ring token's ≥3:1 ratios; THIS holds the
// wiring in app.css, because the audit's findings were a class, not one
// element:
//   1. the global :focus-visible outline must ride --color-focus-ring
//      (accent alone is ~1.8:1 on dark surfaces — judged invisible);
//   2. @xyflow/react resets `outline: none` on every focusable canvas
//      element (nodes, edges, selection rects) in a stylesheet imported
//      BEFORE app.css — the counter-rules below restore the indicator, and
//      they win the cascade only while they stay after the resets at equal
//      specificity.
// If any piece is dropped, the matching assertion names it.
const css = readFileSync(new URL("../../app.css", import.meta.url), "utf8");

describe("a11y / focus ring wiring", () => {
  it("global :focus-visible outline uses the focus-ring token", () => {
    expect(css).toMatch(
      /^:focus-visible \{\n {2}outline: 2px solid var\(--color-focus-ring\);/m,
    );
  });

  it("counter-rules cover every xyflow element whose outline xyflow resets", () => {
    // Mirrors the reset selectors in @xyflow/react/dist/style.css:
    // .react-flow__node.selectable:focus{,.focus-visible}, .react-flow__edge
    // (.selected/:focus/:focus-visible) and the two selection rects.
    for (const selector of [
      ".react-flow__node.selectable:focus-visible",
      ".react-flow__edge:focus-visible",
      ".react-flow__edge.selectable:focus-visible",
      ".react-flow__nodesselection-rect:focus-visible",
      ".react-flow__selection:focus-visible",
    ]) {
      expect(css, `missing counter-rule selector: ${selector}`).toContain(
        selector,
      );
    }
    expect(css).toMatch(
      /\.react-flow__nodesselection-rect:focus-visible,[^{]+\{[^}]*outline: 2px solid var\(--color-focus-ring\);[^}]*\}/s,
    );
  });

  it("counter-rule for react-virtuoso's inline outline:none carries !important", () => {
    // Inline `outline: none` on the focusable scroller beats any
    // cascade position; only !important restores the ring there.
    expect(css).toMatch(
      /\[data-virtuoso-scroller\]:focus-visible \{[^}]*outline: 2px solid var\(--color-focus-ring\) !important;[^}]*\}/s,
    );
  });

  it("pins the token values to the audited palette (contrast.test.ts audits its own mirror, not this file)", () => {
    // contrast.test.ts holds a hand-mirrored hex table; retuning the token
    // here would leave that table stale and the original NC back, green.
    expect(css.match(/--color-focus-ring: #818cf8;/g)).toHaveLength(2); // @theme + dark
    expect(css.match(/--color-focus-ring: #4f46e5;/g)).toHaveLength(1); // light
  });

  it("app.css is imported after xyflow's stylesheet (the counter-rules win the cascade by position)", () => {
    // The counter-rules match xyflow's resets at equal specificity, so
    // their effect rests on import order in main.tsx; a bundler emitting
    // xyflow's css after app.css would kill the ring silently.
    const main = readFileSync(new URL("../../main.tsx", import.meta.url), "utf8");
    const xyflow = main.indexOf("@xyflow/react/dist/style.css");
    const app = main.indexOf('import "./app.css"');
    expect(xyflow).toBeGreaterThanOrEqual(0);
    expect(app).toBeGreaterThan(xyflow);
  });

  it("declares --color-focus-ring for the default theme and both overrides", () => {
    // @theme (dark defaults) + :root[data-theme="dark"] + light.
    expect(css.match(/--color-focus-ring:/g)).toHaveLength(3);
  });
});
