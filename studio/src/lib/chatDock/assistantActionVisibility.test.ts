import { describe, expect, it } from "vitest";

import { shouldRenderAssistantActionOffer } from "./assistantActionVisibility";

describe("assistant action offer visibility", () => {
  it("allows a chat pause even when no human question was reconstructed", () => {
    expect(
      shouldRenderAssistantActionOffer({
        runStatus: "paused_waiting_human",
        hasPendingHumanQuestion: false,
        pendingIsAskUser: false,
      }),
    ).toBe(true);
  });

  it("keeps the established ordinary chat-pause path", () => {
    expect(
      shouldRenderAssistantActionOffer({
        runStatus: "paused_waiting_human",
        hasPendingHumanQuestion: true,
        pendingIsAskUser: false,
      }),
    ).toBe(true);
  });

  it("does not show a previous action during an ask_user pause", () => {
    expect(
      shouldRenderAssistantActionOffer({
        runStatus: "paused_waiting_human",
        hasPendingHumanQuestion: true,
        pendingIsAskUser: true,
      }),
    ).toBe(false);
  });

  it("does not show actions while the assistant is still running", () => {
    expect(
      shouldRenderAssistantActionOffer({
        runStatus: "running",
        hasPendingHumanQuestion: false,
        pendingIsAskUser: false,
      }),
    ).toBe(false);
  });
});
