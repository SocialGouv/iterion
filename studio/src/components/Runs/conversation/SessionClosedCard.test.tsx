// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { SessionClosedMessage } from "@/lib/runChat/types";

import SessionClosedCard from "./SessionClosedCard";

afterEach(cleanup);

function mkMessage(reason: SessionClosedMessage["reason"]): SessionClosedMessage {
  return {
    id: `session-closed-${reason}`,
    kind: "session-closed",
    reason,
  };
}

describe("SessionClosedCard", () => {
  it("points the operator at the report tab when the run finishes cleanly", () => {
    render(<SessionClosedCard message={mkMessage("finished")} />);
    expect(
      screen.getByText(/Run finished\. Pick a node above to see its output/i),
    ).toBeTruthy();
    expect(screen.getByText(/Report tab/i)).toBeTruthy();
  });

  it("redirects to the hint banner on failure", () => {
    render(<SessionClosedCard message={mkMessage("failed")} />);
    expect(
      screen.getByText(/Run failed\. Check the Hint banner above the timeline/i),
    ).toBeTruthy();
  });

  it("mentions Resume on cancellation", () => {
    render(<SessionClosedCard message={mkMessage("cancelled")} />);
    expect(
      screen.getByText(/Run cancelled\. Use Resume in the header/i),
    ).toBeTruthy();
  });

  it("renders as a live region so screen readers announce the closure", () => {
    const { container } = render(
      <SessionClosedCard message={mkMessage("finished")} />,
    );
    const node = container.querySelector('[role="status"]');
    expect(node).not.toBeNull();
  });
});
