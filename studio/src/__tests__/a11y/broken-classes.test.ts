import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

import { hit, jsxClassed } from "./_jsx";

describe("phantom colour classes — the allowlist drawn from the theme", () => {
  it("every colour utility resolves to a --color token the build compiles from", () => {
    // The denylist above only sees named shapes; a phantom class is an
    // ABSENCE — no --color token, so Tailwind generates no rule and the
    // element silently inherits. The oracle is the @theme block of
    // app.css: the same source the build compiles utilities from.
    const css = readFileSync(resolve(process.cwd(), "src/app.css"), "utf8");
    const colors = new Set(
      [...css.matchAll(/--color-([a-zA-Z0-9-]+)/g)].map((m) => m[1]!.toLowerCase()),
    );
    const typography = new Set([
      ...[...css.matchAll(/--text-([a-zA-Z0-9-]+)/g)].map((m) => m[1]!.toLowerCase()),
      // Tailwind's default size scale stays generable without a theme token.
      "xs", "sm", "base", "lg", "xl", "2xl", "3xl", "4xl",
    ]);
    // text-X is overloaded: alignment and numeric sizes are not colours.
    const notColour = new Set([
      "center", "left", "right", "justify", "start", "end", "wrap", "nowrap",
      // border side suffixes: border-t / border-b / border-x / border-y.
      "t", "b", "l", "r", "x", "y",
      "white", "black", "transparent", "inherit", "current",
      // Gradient directions and table borders are not colours.
      "collapse", "separate", "inset",
    ]);
    const isNumeric = (name: string) => /^\d/.test(name) || /^\[[^\]]*\]$/.test(name);
    // border side (border-t), side+width (border-l-2), and side+colour
    // (border-t-transparent) are geometry, not colours.
    const isSide = (name: string) =>
      /^[tblrxy](-\d+)?$/.test(name) || /^[tblrxy]-[a-zA-Z]/.test(name);
    // bg-gradient-to-b is a direction, ring-inset is geometry.
    const isGradient = (name: string) => name.startsWith("gradient-to-");
    const isStyle = (name: string) =>
      /^(?:dashed|solid|dotted|double|hidden|none)$/.test(name);
    const hits = jsxClassed().flatMap((e) =>
      e.variants.flatMap((v) =>
        v
          .split(/\s+/)
          // Variant prefixes (hover:, md:, group-hover:) do not change the
          // utility family; strip them, then the token is family-name.
          .map((token) => token.replace(/^(?:[a-z-]+:)+/, ""))
          .filter((token) => /^(?:text|bg|border|ring|fill|stroke)-[a-zA-Z]/.test(token))
          .map((token) =>
            token
              .replace(/^(?:text|bg|border|ring|fill|stroke)-/, "")
              // The alpha suffix (surface-1/90, info/[0.07]) is not part of
              // the token name.
              .replace(/\/(?:\d+|\[[^\]]*\])$/, ""),
          )
          .filter(
            (name) =>
              !colors.has(name.toLowerCase()) &&
              !typography.has(name.toLowerCase()) &&
              !notColour.has(name) &&
              !isNumeric(name) &&
              !isSide(name) &&
              !isGradient(name) &&
              !isStyle(name),
          )
          .map((name) => hit(e, `${name} (phantom: no --color-${name} token)`)),
      ),
    );
    if (hits.length) {
      throw new Error(
        `colour classes with no theme token — Tailwind generates no rule and the element inherits:\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });
});
