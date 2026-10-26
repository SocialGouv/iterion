import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

// RGAA 7.4 (#2513): the workflow switcher redraws the canvas on change —
// its Select carries an accessible name. The full Toolbar mount drags the
// document store and the editor-tab hooks; the pin holds the naming at the
// source (the colocated editor suites cover the switcher's behaviour).
describe("Toolbar workflow switcher (RGAA 7.4, #2513)", () => {
  it("names the workflow Select", () => {
    const src = readFileSync(
      resolve(process.cwd(), "src/components/Toolbar/Toolbar.tsx"),
      "utf8",
    );
    expect(src).toContain('aria-label="Active workflow (switching redraws the canvas)"');
  });
});
