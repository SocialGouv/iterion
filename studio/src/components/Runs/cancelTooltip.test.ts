// @vitest-environment jsdom
import { describe, expect, it } from "vitest";

import { cancelTooltip } from "./RunHeader";

// The helper is plain text, but RunHeader.tsx pulls in wouter and React
// modules that some browsers expect at module-eval time. The jsdom env
// is the safe choice for the import, even though the tested function
// itself never touches the DOM.
describe("cancelTooltip", () => {
  it("explains the queue-drop case", () => {
    expect(cancelTooltip("queued")).toMatch(/queue/i);
  });

  it("warns that paused runs terminate when cancelled", () => {
    expect(cancelTooltip("paused_waiting_human")).toMatch(/terminates/i);
    expect(cancelTooltip("paused_operator")).toMatch(/terminates/i);
  });

  it("mentions the safe-boundary stop for running runs", () => {
    expect(cancelTooltip("running")).toMatch(/safe boundary/i);
  });

  it("falls back to a generic message for terminal states", () => {
    expect(cancelTooltip("finished")).toBe("Cancel this run.");
    expect(cancelTooltip("failed")).toBe("Cancel this run.");
  });
});
