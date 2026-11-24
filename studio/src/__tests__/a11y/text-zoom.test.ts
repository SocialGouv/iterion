import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import {
  SIDEBAR_W_COLLAPSED,
  SIDEBAR_W_EXPANDED,
  sidebarWidthPx,
} from "@/components/shared/sidebarWidth";

// RGAA 10.4.1 (#2522): at 200 % text size the primary nav labels must stay
// fully readable — they WRAP instead of truncating, and the sidebar rail's
// width is font-relative (rem) so it grows with the text instead of cutting
// it. The floating assistant's resize clamp needs pixels: those are derived
// from the same rem literals through the live root font size, never
// duplicated as a px constant text zoom would silently leave behind.
// Source pins follow the idiom of source-discipline.test.ts.

const sidebarSrc = readFileSync(
  resolve(process.cwd(), "src/components/shared/Sidebar.tsx"),
  "utf8",
);
const navLinksSrc = readFileSync(
  resolve(process.cwd(), "src/components/shared/NavLinks.tsx"),
  "utf8",
);
const chatDockSrc = readFileSync(
  resolve(process.cwd(), "src/components/ChatDock/ChatDock.tsx"),
  "utf8",
);

describe("text stays readable at 200 % (RGAA 10.4.1, #2522)", () => {
  it("the sidebar widths are rem literals, not px — the rail grows with the text", () => {
    for (const widthClass of [SIDEBAR_W_EXPANDED, SIDEBAR_W_COLLAPSED]) {
      expect(widthClass).toMatch(/^w-\[\d+(?:\.\d+)?rem\]$/);
    }
    // The aside consumes those classes — no fixed px width of its own.
    expect(sidebarSrc).not.toMatch(/w-\[\d+px\]/);
  });

  it("the primary nav labels wrap — the label span never truncates", () => {
    // The expanded label is the `<span …>{label}</span>` of a NavItem; the
    // collapsed rail renders the icon alone, so nothing is lost there.
    const labelSpans = [
      ...navLinksSrc.matchAll(/<span className="([^"]*)">\{label\}<\/span>/g),
    ].map((m) => m[1]);
    expect(labelSpans.length).toBeGreaterThan(0);
    for (const classes of labelSpans) {
      expect(classes).not.toMatch(/\btruncate\b/);
    }
  });

  it("the assistant dock's pixel width is derived from the rem literal, not duplicated", () => {
    // No numeric px mirror beside the width classes (the pre-fix drift:
    // SIDEBAR_EXPANDED_PX = 220 typed next to w-[220px]).
    expect(sidebarSrc).not.toMatch(/SIDEBAR_(?:EXPANDED|COLLAPSED)_PX\s*=\s*\d+/);
    // ChatDock consumes the derived value, not a constant of its own.
    expect(chatDockSrc).toContain("sidebarWidthPx");
    expect(chatDockSrc).not.toMatch(/SIDEBAR_(?:EXPANDED|COLLAPSED)_PX/);
    // The derivation itself: 13.75rem / 3.5rem at the default 16px root.
    expect(sidebarWidthPx(false)).toBe(13.75 * 16);
    expect(sidebarWidthPx(true)).toBe(3.5 * 16);
  });
});
