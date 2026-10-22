import { describe, expect, it } from "vitest";
import { tokenBlocks } from "./_cssTokens";

// The token reader behind the mirror guard, on hand-written CSS: the
// shape app.css has is read; every valid CSS shape that would change what
// ships without changing what the reader sees is refused by name.

const SHAPE = `@import "tailwindcss";
@custom-variant dark (&:where([data-theme="dark"], [data-theme="dark"] *));
@theme {
  --font-sans: "Geist", sans-serif;
  --color-accent: #4f46e5; /* indigo-600 */
  --color-danger-soft: rgba(220, 38, 38, 0.18);
}
:root[data-theme="dark"] {
  --color-accent: #4f46e5;
  --color-danger-soft: rgba(220, 38, 38, 0.18);
}
:root[data-theme='light']{--color-accent:#4338ca;--color-danger-soft: rgba(185, 28, 28, 0.10)}
.card { color: var(--color-accent); }
`;

describe("a11y / app.css token reader", () => {
  it("reads the three blocks, quotes or not, braces glued or not", () => {
    const b = tokenBlocks(SHAPE);
    expect(b.problems).toEqual([]);
    expect(b.theme).toEqual({ accent: "#4f46e5", "danger-soft": "rgba(220, 38, 38, 0.18)" });
    expect(b.dark).toEqual({ accent: "#4f46e5", "danger-soft": "rgba(220, 38, 38, 0.18)" });
    expect(b.light).toEqual({ accent: "#4338ca", "danger-soft": "rgba(185, 28, 28, 0.10)" });
  });

  it("does not read a declaration inside a comment", () => {
    const b = tokenBlocks(SHAPE.replace("--color-accent: #4f46e5; /* indigo-600 */", "--color-accent: #4f46e5; /* was: --color-accent: #000000; */"));
    expect(b.problems).toEqual([]);
    expect(b.theme.accent).toBe("#4f46e5");
  });

  it("holds the last of two same-theme blocks, as the cascade does", () => {
    const b = tokenBlocks(`${SHAPE}\n:root[data-theme="dark"] { --color-accent: #111111; }\n`);
    expect(b.problems).toEqual([]);
    expect(b.dark.accent).toBe("#111111");
  });

  it("refuses a token declared in a plain :root", () => {
    const b = tokenBlocks(`${SHAPE}\n:root { --color-accent: #000000; }\n`);
    expect(b.problems).toEqual([expect.stringMatching(/^line 15: --color-accent is declared in `:root`/)]);
  });

  it("refuses a theme block nested in @layer", () => {
    const b = tokenBlocks(`${SHAPE}\n@layer base {\n  :root[data-theme="dark"] { --color-accent: #000000; }\n}\n`);
    expect(b.problems).toEqual([expect.stringMatching(/^line 16: --color-accent is declared in `@layer base`/)]);
  });

  it("refuses a theme block nested in @media", () => {
    const b = tokenBlocks(`${SHAPE}\n@media print {\n  :root[data-theme="light"] { --color-accent: #000000; }\n}\n`);
    expect(b.problems).toEqual([expect.stringMatching(/^line 16: --color-accent is declared in `@media print`/)]);
  });

  it("refuses an !important declaration, whatever its case or spacing", () => {
    for (const bang of ["!important", "!IMPORTANT", "! important"]) {
      const b = tokenBlocks(SHAPE.replace("--color-accent: #4f46e5;\n  --color-danger-soft", `--color-accent: #4f46e5 ${bang};\n  --color-danger-soft`));
      expect(b.problems, bang).toEqual([expect.stringMatching(/^line 9: --color-accent is !important/)]);
    }
  });

  it("refuses a declaration nested inside a theme block (an @media or a selector it wraps)", () => {
    // Lightning CSS hoists the nested at-rule to the top level: the value
    // is not the block's own, and the reader must not hold it as such.
    const b = tokenBlocks(SHAPE.replace("  --color-accent: #4f46e5;\n  --color-danger-soft", "  --color-accent: #4f46e5;\n  @media (prefers-contrast: more) { --color-accent: #000000; }\n  --color-danger-soft"));
    expect(b.problems).toEqual([expect.stringMatching(/^line 10: --color-accent is declared in a rule nested inside `:root\[data-theme="dark"\]`/)]);
    expect(b.dark.accent).toBe("#4f46e5");
  });

  it("does not let a brace inside a string close a block", () => {
    const b = tokenBlocks(`${SHAPE}.x::after { content: "}"; }\n* { --color-accent: #000000; }\n`);
    expect(b.problems).toEqual([expect.stringMatching(/^line 15: --color-accent is declared in `\*`/)]);
  });

  it("names a missing block", () => {
    const b = tokenBlocks(SHAPE.replace(/:root\[data-theme='light'\]\{[^}]*\}/, ""));
    expect(b.problems).toEqual(['no top-level :root[data-theme="light"] block']);
  });
});
