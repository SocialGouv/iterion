// @vitest-environment jsdom
//
// Cross-review is real money — a full extra model call per turn, for the whole
// conversation — so the choice is offered before a conversation starts rather
// than buried. These tests pin the two properties that make that safe.
import { beforeEach, describe, expect, it } from "vitest";

import {
  ASSISTANT_ASK_BEFORE_START_KEY,
  ASSISTANT_REVIEWER_KEY,
  botDeclaresReviewer,
  readAskBeforeStart,
  readReviewer,
  reviewerVars,
  writeAskBeforeStart,
  writeReviewer,
} from "./assistantPrefs";

beforeEach(() => window.localStorage.clear());

describe("assistant start preferences", () => {
  it("does not cross-review unless asked — the default cannot cost money", () => {
    expect(readReviewer()).toBe(false);
  });

  it("offers the choice on a fresh browser", () => {
    expect(readAskBeforeStart()).toBe(true);
  });

  it("remembers both", () => {
    writeReviewer(true);
    writeAskBeforeStart(false);
    expect(readReviewer()).toBe(true);
    expect(readAskBeforeStart()).toBe(false);
  });

  // The trap this avoids: "don't ask again" reading as "forget my answer".
  // Dismissing the prompt must stop the QUESTION, not the setting.
  it("keeps the saved answer after the prompt is dismissed", () => {
    writeReviewer(true);
    writeAskBeforeStart(false);
    expect(readReviewer()).toBe(true);
    expect(reviewerVars(readReviewer())).toEqual({ reviewer: "on" });
  });

  it("survives a corrupt stored value rather than turning itself on", () => {
    window.localStorage.setItem(ASSISTANT_REVIEWER_KEY, "yes-please");
    expect(readReviewer()).toBe(false);
    window.localStorage.setItem(ASSISTANT_ASK_BEFORE_START_KEY, "maybe");
    expect(readAskBeforeStart()).toBe(false);
  });

  it("states the choice explicitly both ways", () => {
    // Never omission: an operator's explicit "off" must not be left to a bot's
    // own default, which may differ.
    expect(reviewerVars(false)).toEqual({ reviewer: "off" });
    expect(reviewerVars(true)).toEqual({ reviewer: "on" });
  });
});

// The var is only sent to a bot whose manifest declares it. Guessing which
// bots support what is exactly what the manifest registry exists to stop.
describe("which bots are offered cross-review", () => {
  it("recognises a bot that declares it", () => {
    expect(
      botDeclaresReviewer({ launcherVars: [{ name: "reviewer" }] }),
    ).toBe(true);
  });

  it("refuses one that does not", () => {
    expect(botDeclaresReviewer({ launcherVars: [{ name: "scope" }] })).toBe(false);
    expect(botDeclaresReviewer({ launcherVars: [] })).toBe(false);
    expect(botDeclaresReviewer({})).toBe(false);
  });
});
