import { describe, expect, it } from "vitest";

import { emptyCopy } from "./RunConversationView";

describe("RunConversationView.emptyCopy", () => {
  it("explains the wait when the run is queued", () => {
    expect(emptyCopy("queued")).toMatch(/queued run/i);
  });

  it("acknowledges active runs that haven't emitted events yet", () => {
    expect(emptyCopy("running")).toMatch(/first node event/i);
  });

  it("makes the finished-but-empty case explicit", () => {
    expect(emptyCopy("finished")).toMatch(/finished without producing/i);
  });

  it("groups failure variants behind a shared explanatory line", () => {
    expect(emptyCopy("failed")).toMatch(/Events tab/i);
    expect(emptyCopy("failed_resumable")).toMatch(/Events tab/i);
  });

  it("calls out the cancellation case so the silence isn't mysterious", () => {
    expect(emptyCopy("cancelled")).toMatch(/cancelled/i);
  });

  it("points to the form when paused waiting for human input", () => {
    expect(emptyCopy("paused_waiting_human")).toMatch(/form below/i);
  });

  it("points at the Resume button when paused by an operator", () => {
    expect(emptyCopy("paused_operator")).toMatch(/Resume/);
  });

  it("falls back to the generic pre-flight line when status is unknown", () => {
    expect(emptyCopy(null)).toMatch(/Waiting/i);
  });
});
