import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

// RGAA 13.5: the header's ● (dispatch-eligible) and ✓ (terminal) glyphs
// carry their meaning as sr-only text — the bare glyph is decorative.
// Pinned as a source contract: mounting the full Column drags the card
// stack (IssueCard) into the test for two spans, and the contract is the
// file's, not one render's.
const src = readFileSync(resolve(process.cwd(), "src/views/Board/Column.tsx"), "utf8");

describe("board column header glyphs", () => {
  it("pairs each status glyph with its sr-only text alternative", () => {
    const pairs = [
      { glyph: "●", alt: "dispatch eligible", cls: "text-success" },
      { glyph: "✓", alt: "terminal column", cls: "text-fg-muted" },
    ];
    for (const { glyph, alt, cls } of pairs) {
      const gated = new RegExp(
        `aria-hidden="true" className="ml-1 ${cls}">${glyph}</span>\\s*<span className="sr-only">${alt}</span>`,
      );
      if (!gated.test(src)) {
        throw new Error(
          `the ${glyph} glyph is not aria-hidden with its "${alt}" sr-only alternative — a screen reader hears nothing between the count and the menu`,
        );
      }
    }
  });

  it("leaves no bare status glyph anywhere in the header", () => {
    for (const glyph of ["●", "✓"]) {
      // The gated span ENDS with >glyph</span> too — exclude a span whose
      // opening carries aria-hidden, flag only a truly bare one.
      const bare = new RegExp(`(?<!aria-hidden="true" )<span className="ml-1 [^"]*">${glyph}</span>`);
      if (bare.test(src)) {
        throw new Error(
          `a bare ${glyph} span renders without aria-hidden — pair it with its sr-only alternative`,
        );
      }
    }
    expect(true).toBe(true);
  });
});
