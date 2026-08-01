// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";

import {
  ASSISTANT_DOCK_KEY,
  readDockState,
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
