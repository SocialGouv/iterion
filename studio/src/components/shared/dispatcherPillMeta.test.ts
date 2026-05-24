import { describe, expect, it } from "vitest";

import { dispatcherPillMeta, type DispatcherPillState } from "./dispatcherPillMeta";

describe("dispatcherPillMeta", () => {
  it("labels the four lifecycle states as 'dispatcher: <state>'", () => {
    expect(dispatcherPillMeta("idle").label).toBe("dispatcher: idle");
    expect(dispatcherPillMeta("running").label).toBe("dispatcher: running");
    expect(dispatcherPillMeta("paused").label).toBe("dispatcher: paused");
    expect(dispatcherPillMeta("error").label).toBe("dispatcher: error");
  });

  it("labels the synthetic unreachable state distinctly", () => {
    expect(dispatcherPillMeta("unreachable").label).toBe("unreachable");
  });

  it("keeps an unrecognised runtime state's real name instead of relabeling it idle", () => {
    // Defensive path for server/UI enum skew — a state outside the typed
    // union must not be silently shown as "idle" (which would also
    // mis-offer the Start button in the control bar).
    const meta = dispatcherPillMeta("starting" as DispatcherPillState);
    expect(meta.label).toBe("dispatcher: starting");
    expect(meta.label).not.toBe("dispatcher: idle");
  });

  it("gives every state — including idle and unreachable — a hover explanation", () => {
    // The whole point of the helper: the two states that previously
    // rendered with no tooltip now explain themselves.
    expect(dispatcherPillMeta("idle").title).toMatch(/Start/i);
    expect(dispatcherPillMeta("unreachable").title).toMatch(/iterion dispatch/i);
    for (const s of [
      "idle",
      "running",
      "paused",
      "error",
      "unreachable",
    ] as const) {
      expect(dispatcherPillMeta(s).title.length).toBeGreaterThan(0);
    }
  });

  it("maps each state to a background + foreground colour class", () => {
    expect(dispatcherPillMeta("running").className).toMatch(/green/);
    expect(dispatcherPillMeta("paused").className).toMatch(/amber/);
    expect(dispatcherPillMeta("error").className).toMatch(/red/);
    expect(dispatcherPillMeta("unreachable").className).toMatch(/red/);
    expect(dispatcherPillMeta("idle").className).toMatch(/fg-muted/);
  });
});
