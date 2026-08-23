// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";

import {
  ASSISTANT_DOCK_KEY,
  ASSISTANT_DOCK_WIDTH_KEY,
  DOCKED_WIDTH_DEFAULT_PX,
  readDockSize,
  readDockState,
  readDockWidth,
  writeDockSize,
  writeDockState,
} from "./dockState";

beforeEach(() => window.localStorage.clear());

describe("assistant dock persistence", () => {
  // Persisted per USER under one key, not per route: docking on /board
  // must leave it docked on /runs.
  it("round-trips through the single assistant key", () => {
    writeDockState(ASSISTANT_DOCK_KEY, "docked-right");
    expect(readDockState(ASSISTANT_DOCK_KEY)).toBe("docked-right");
    writeDockState(ASSISTANT_DOCK_KEY, "closed");
    expect(readDockState(ASSISTANT_DOCK_KEY)).toBe("closed");
  });

  it("falls back rather than restoring a value from an older build", () => {
    window.localStorage.setItem(ASSISTANT_DOCK_KEY, "docked-left");
    expect(readDockState(ASSISTANT_DOCK_KEY)).toBe("closed");
  });

  // The run console's steering dock has its own key; docking one must
  // not move the other.
  it("does not share the run console's key", () => {
    writeDockState(ASSISTANT_DOCK_KEY, "floating");
    expect(readDockState("run-console-v2.chat-dock")).toBe("closed");
  });
});

// The docked width is what AppShell reserves, so a stored value has to survive
// a reload — and has to be re-clamped on the way back, because the monitor
// that produced it may not be the monitor reading it.
describe("dock width persistence", () => {
  it("round-trips a width the operator dragged to", () => {
    window.localStorage.setItem(ASSISTANT_DOCK_WIDTH_KEY, "520");
    expect(readDockWidth(DOCKED_WIDTH_DEFAULT_PX, 1600)).toBe(520);
  });

  it("re-clamps a width stored on a wider screen", () => {
    window.localStorage.setItem(ASSISTANT_DOCK_WIDTH_KEY, "1400");
    expect(readDockWidth(DOCKED_WIDTH_DEFAULT_PX, 1000)).toBe(700);
  });

  it("falls back when nothing is stored", () => {
    expect(readDockWidth(DOCKED_WIDTH_DEFAULT_PX, 1600)).toBe(
      DOCKED_WIDTH_DEFAULT_PX,
    );
  });

  it("ignores a corrupt value rather than collapsing the column", () => {
    window.localStorage.setItem(ASSISTANT_DOCK_WIDTH_KEY, "not-a-number");
    expect(readDockWidth(DOCKED_WIDTH_DEFAULT_PX, 1600)).toBe(
      DOCKED_WIDTH_DEFAULT_PX,
    );
  });
});

describe("floating size persistence", () => {
  it("round-trips the size the browser handle produced", () => {
    writeDockSize("k", { width: 640, height: 700 });
    expect(readDockSize("k", { width: 420, height: 520 })).toEqual({
      width: 640,
      height: 700,
    });
  });

  it("falls back when nothing is stored", () => {
    expect(readDockSize("missing", { width: 420, height: 520 })).toEqual({
      width: 420,
      height: 520,
    });
  });
});
