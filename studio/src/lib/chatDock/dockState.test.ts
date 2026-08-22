import { describe, expect, it } from "vitest";

import {
  DOCK_BREAKPOINT_PX,
  DOCK_STATES,
  openedDock,
  readDockState,
  STEERING_DOCK_BREAKPOINT_PX,
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
