// @vitest-environment jsdom
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup } from "@testing-library/react";
import { readBooleanFlag } from "@/lib/localStorageFlag";
import { useUIStore } from "@/store/ui";
import { files } from "./_scanner";

// The in-page motion setting (RGAA 13.8): infinite animations run by
// default and collapse under the root [data-motion="reduced"] attribute
// the store mirrors. The OS-level prefers-reduced-motion reset stays a
// separate mechanism; this is the page control the criterion asks for.
const KEY = "iterion.motionReduced";

// app.css read from disk, the way focus-ring.test.ts reads it: the reset
// block this file witnesses lives in the stylesheet, and Vite's ?raw
// import returns an empty string for CSS in this setup.
const appCss = readFileSync(resolve(process.cwd(), "src/app.css"), "utf8");


afterEach(() => {
  cleanup();
  useUIStore.getState().setMotionReduced(false);
});

describe("motion preference", () => {
  it("mirrors the setting onto the document root and persists", () => {
    useUIStore.getState().setMotionReduced(false);
    expect(useUIStore.getState().motionReduced).toBe(false);
    expect(document.documentElement.hasAttribute("data-motion")).toBe(false);

    useUIStore.getState().setMotionReduced(true);
    expect(useUIStore.getState().motionReduced).toBe(true);
    expect(document.documentElement.getAttribute("data-motion")).toBe("reduced");
    expect(readBooleanFlag(KEY, false)).toBe(true);

    useUIStore.getState().setMotionReduced(false);
    expect(document.documentElement.hasAttribute("data-motion")).toBe(false);
    expect(readBooleanFlag(KEY, false)).toBe(false);
  });

  it("registers every known infinite animation under the reduced reset", () => {
    // app.css's [data-motion="reduced"] block is the single gate: a new
    // infinite animation utility must be listed there or the setting
    // silently misses it.
    // The selector list spans lines and sits BEFORE the brace: capture the
    // list itself, not the block body.
    const gated = /\[data-motion="reduced"\][^{]*?(?=\{)/.exec(appCss)?.[0] ?? "";
    for (const sel of [".animate-pulse", ".animate-ping", ".animate-caret-blink", ".ch-stack-track"]) {
      if (!gated.includes(sel)) {
        throw new Error(`the reduced-motion reset does not register ${sel} — the setting would silently miss it`);
      }
    }
  });

  it("every inline infinite animation sits behind the motion gate", () => {
    const ungated = files
      .filter(([p, src]) => p.endsWith(".tsx") && /animation:\s*[^;]*infinite/.test(src))
      .filter(([p, src]) => !/motionReduced/.test(src))
      .map(([p]) => p);
    if (ungated.length) {
      throw new Error(
        `inline infinite animations outside the motion gate (gate them with the ui store's motionReduced or register the class in app.css):\n${ungated.join("\n")}`,
      );
    }
    expect(ungated).toHaveLength(0);
  });
});
