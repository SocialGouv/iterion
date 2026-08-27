import { describe, expect, it } from "vitest";

import {
  DOCK_BREAKPOINT_PX,
  DOCK_STATES,
  openedDock,
  readDockState,
  STEERING_DOCK_BREAKPOINT_PX,
  DOCK_MIN_WIDTH_PX,
  FLOATING_MIN_HEIGHT_PX,
  FLOATING_MIN_WIDTH_PX,
  clampDockSize,
  clampDockWidth,
} from "./dockState";

describe("openedDock", () => {
  it("floats on a wide viewport", () => {
    expect(openedDock(DOCK_BREAKPOINT_PX + 1)).toBe("floating");
    expect(openedDock(1920)).toBe("floating");
  });

  it("docks at and below the lg breakpoint", () => {
    expect(openedDock(DOCK_BREAKPOINT_PX)).toBe("docked-right");
    expect(openedDock(768)).toBe("docked-right");
  });

  it("keeps the steering panel on its wider canvas breakpoint", () => {
    expect(openedDock(900, STEERING_DOCK_BREAKPOINT_PX)).toBe("docked-right");
    expect(openedDock(1100, STEERING_DOCK_BREAKPOINT_PX)).toBe("floating");
  });
});

describe("readDockState", () => {
  // Storage is unavailable in the node test environment, which is the
  // same path a private-mode browser takes — the fallback must hold
  // rather than throwing into the render.
  it("falls back when storage is unreadable", () => {
    expect(readDockState("iterion.test.absent")).toBe("closed");
    expect(readDockState("iterion.test.absent", "floating")).toBe("floating");
  });
});

describe("DOCK_STATES", () => {
  it("is the allowlist readDockState validates against", () => {
    expect([...DOCK_STATES]).toEqual(["closed", "floating", "docked-right"]);
  });
});

// The docked column's width is not a cosmetic preference: AppShell reserves
// exactly this many pixels so the dock pushes the page aside instead of
// covering it. The bounds are what keep that promise true.
describe("dock width", () => {
  it("keeps a width the operator chose", () => {
    expect(clampDockWidth(520, 1600)).toBe(520);
  });

  it("refuses to shrink below a usable column", () => {
    expect(clampDockWidth(80, 1600)).toBe(DOCK_MIN_WIDTH_PX);
  });

  it("refuses to swallow the page it is supposed to sit beside", () => {
    // 70% ceiling: past it, "the page is what's left" stops being true.
    expect(clampDockWidth(5000, 1000)).toBe(700);
  });
});

describe("floating panel size", () => {
  it("never restores a panel larger than the viewport", () => {
    expect(
      clampDockSize({ width: 9999, height: 9999 }, { width: 900, height: 600 }),
    ).toEqual({ width: 900, height: 600 });
  });

  it("keeps the panel usable at the small end", () => {
    expect(
      clampDockSize({ width: 10, height: 10 }, { width: 900, height: 600 }),
    ).toEqual({ width: FLOATING_MIN_WIDTH_PX, height: FLOATING_MIN_HEIGHT_PX });
  });
});

// The floating panel is pinned BOTTOM-RIGHT, which is what made the browser's
// own resize handle useless: at the bottom-right corner, dragging it grew the
// panel off the screen, so the operator could only ever shrink it. The handle
// is at the top-left instead, and these are the bounds it works within.
describe("growing the floating panel", () => {
  it("grows when dragged up and to the left", () => {
    // The handle adds (startX - x) and (startY - y): dragging outward.
    const start = { width: 420, height: 520 };
    const grown = clampDockSize(
      { width: start.width + 120, height: start.height + 80 },
      { width: 1600, height: 1000 },
    );
    expect(grown).toEqual({ width: 540, height: 600 });
  });

  it("stops at the viewport rather than growing off-screen", () => {
    expect(
      clampDockSize({ width: 5000, height: 5000 }, { width: 800, height: 600 }),
    ).toEqual({ width: 800, height: 600 });
  });

  it("stops at a usable minimum when shrunk", () => {
    expect(
      clampDockSize({ width: 10, height: 10 }, { width: 1600, height: 1000 }),
    ).toEqual({ width: FLOATING_MIN_WIDTH_PX, height: FLOATING_MIN_HEIGHT_PX });
  });
});

// The panel is pinned bottom-right, so the box it may grow into is the
// viewport MINUS its own offsets. Clamping against the raw viewport let a
// full-width panel start at a negative x and run off the LEFT edge — the one
// direction a bottom-right-anchored panel can escape.
describe("the floating panel stays on screen", () => {
  // What the shell passes: innerWidth - rightOffset - margin.
  const available = { width: 1920 - 24 - 16, height: 1080 - 16 - 16 };

  it("stops at the space actually available, not at the viewport", () => {
    const got = clampDockSize({ width: 5000, height: 5000 }, available);
    expect(got.width).toBe(1880);
    expect(got.height).toBe(1048);
    // The decisive property: it still fits beside its own right offset.
    expect(got.width + 24 + 16).toBeLessThanOrEqual(1920);
  });

  it("keeps a panel restored from a bigger monitor on screen", () => {
    const small = { width: 900 - 24 - 16, height: 700 - 16 - 16 };
    const got = clampDockSize({ width: 1600, height: 1200 }, small);
    expect(got.width).toBeLessThanOrEqual(small.width);
    expect(got.height).toBeLessThanOrEqual(small.height);
  });

  // Fitting wins over the minimum: on a window narrower than a "usable" panel,
  // overflowing would be worse than being small. The shell never asks for that
  // anyway — it floors the available box at the minimum before clamping — so
  // this pins the helper's own rule, not the behaviour the operator sees.
  it("fits the window even when that is below the usable minimum", () => {
    const got = clampDockSize({ width: 400, height: 400 }, { width: 200, height: 150 });
    expect(got).toEqual({ width: 200, height: 150 });
  });

  it("honours the minimum whenever there is room for it", () => {
    const got = clampDockSize({ width: 10, height: 10 }, { width: 900, height: 700 });
    expect(got.width).toBe(FLOATING_MIN_WIDTH_PX);
    expect(got.height).toBe(FLOATING_MIN_HEIGHT_PX);
  });
});
