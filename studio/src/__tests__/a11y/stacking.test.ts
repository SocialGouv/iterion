import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const appCss = readFileSync(new URL("../../app.css", import.meta.url), "utf8");

function zIndexToken(name: string): number {
  const match = appCss.match(new RegExp(`--z-${name}:\\s*(\\d+)`));
  if (!match) throw new Error(`missing --z-${name} stacking token`);
  return Number(match[1]);
}

describe("global stacking ladder", () => {
  it("keeps persistent docks above page chrome and below modal surfaces", () => {
    const order = [
      "canvas",
      "dock",
      "overlay",
      "modal",
      "confirm",
      "popover",
      "tooltip",
      "toast",
    ].map(zIndexToken);

    for (let index = 1; index < order.length; index += 1) {
      const current = order[index];
      const previous = order[index - 1];
      if (current === undefined || previous === undefined) {
        throw new Error("incomplete stacking ladder");
      }
      expect(current).toBeGreaterThan(previous);
    }
  });
});
