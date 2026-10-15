import { describe, expect, it } from "vitest";

import {
  GATE_OFF_LABEL,
  GATE_ON_LABEL,
  gateValueDisables,
  mergeGateEnabled,
  mergeGateLabel,
} from "./mergeGate";

// The studio's copy of pkg/forge.GateValueDisables. The hazard this guards is
// a studio that says "Advisory only" while the bot still posts the required
// status (or the reverse): the operator reads the posture here and the gate
// machinery arms there, from the SAME value. Every row below is a value the Go
// table classifies, so a drift on either side shows up as a failing test
// rather than as a repo whose screen and whose merge button disagree.

describe("gateValueDisables — the truthy words the gating bots accept", () => {
  for (const on of ["true", "1", "yes", "on", "TRUE", "On", "YES", " true ", "\t1\n"]) {
    it(`keeps the gate on for ${JSON.stringify(on)}`, () => {
      expect(gateValueDisables(on)).toBe(false);
    });
  }

  // Anything that is not affirmatively truthy disables: the runtime coerces
  // "" to false and hands an unparsable value to the bot raw, which fails its
  // own truthy test.
  for (const off of ["false", "0", "no", "off", "", "   ", "maybe", "truthy", "1 1", "ON!"]) {
    it(`disables the gate for ${JSON.stringify(off)}`, () => {
      expect(gateValueDisables(off)).toBe(true);
    });
  }
});

describe("mergeGateEnabled — what an integration's launch_vars pin", () => {
  it("reads an ABSENT gate_enabled as the gate being on", () => {
    expect(mergeGateEnabled(undefined)).toBe(true);
    expect(mergeGateEnabled(null)).toBe(true);
    expect(mergeGateEnabled({})).toBe(true);
    expect(mergeGateEnabled({ review_tier: "guard" })).toBe(true);
  });

  it("honours an explicit pin, either way", () => {
    expect(mergeGateEnabled({ gate_enabled: "true" })).toBe(true);
    expect(mergeGateEnabled({ gate_enabled: "false" })).toBe(false);
    expect(mergeGateEnabled({ gate_enabled: "" })).toBe(false);
  });

  it("labels the posture with the words every site shows", () => {
    expect(mergeGateLabel({ gate_enabled: "false" })).toBe(GATE_OFF_LABEL);
    expect(mergeGateLabel({})).toBe(GATE_ON_LABEL);
    expect(GATE_ON_LABEL).toBe("Gate merges");
    expect(GATE_OFF_LABEL).toBe("Advisory only");
  });
});
