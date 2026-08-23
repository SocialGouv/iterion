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
