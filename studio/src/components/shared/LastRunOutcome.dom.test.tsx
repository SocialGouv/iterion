// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import LastRunOutcome, { runOutcomeFailed, type RunOutcomeRecord } from "./LastRunOutcome";

afterEach(cleanup);

describe("LastRunOutcome", () => {
  it("names the terminal status, the code, the message and the remedy", () => {
    render(
      <LastRunOutcome
        record={{
          last_run_id: "run/7",
          last_run_status: "failed",
          last_run_error_code: "SANDBOX_DRIVER_UNAVAILABLE",
          last_run_error: "sandbox start: no runtime",
        }}
        trailer="The trigger stays armed."
      />,
    );
    expect(screen.getByText("Last run: failed")).toBeTruthy();
    expect(screen.getByText("SANDBOX_DRIVER_UNAVAILABLE")).toBeTruthy();
    expect(screen.getByText("sandbox start: no runtime")).toBeTruthy();
    expect(screen.getByText(/install Docker or Podman/)).toBeTruthy();
    expect(screen.getByText("The trigger stays armed.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "View run" }).getAttribute("href")).toBe("/runs/run%2F7");
  });

  // A cancelled run leaves no message at all. Keyed on the message, the
  // record would read as a success — which is the ambiguity #1501 is about.
  it("treats a terminal status that is not finished as a failure even with no message", () => {
    const cancelled: RunOutcomeRecord = { last_run_status: "cancelled", last_run_id: "run/8" };
    expect(runOutcomeFailed(cancelled)).toBe(true);
    render(<LastRunOutcome record={cancelled} trailer="The trigger stays armed." />);
    const line = screen.getByText("Last run: cancelled");
    expect(line.parentElement?.className).toContain("text-warning-fg");
    // Nothing to advise about: there is no cause, only an operator's stop.
    expect(screen.queryByText(/Open the Events tab/)).toBeNull();
    // …but the record-specific consequence is still said.
    expect(screen.getByText("The trigger stays armed.")).toBeTruthy();
  });

  it("reads a successful run as a success, and keeps a later launch refusal visible", () => {
    const record: RunOutcomeRecord = { last_run_status: "finished", last_error: "quota reached" };
    expect(runOutcomeFailed(record)).toBe(false);
    render(<LastRunOutcome record={record} />);
    const line = screen.getByText("Last run: finished");
    expect(line.parentElement?.className).toContain("text-fg-muted");
    expect(screen.getByText("Last launch refused: quota reached")).toBeTruthy();
  });

  it("stays empty before the first outcome", () => {
    const { container } = render(<LastRunOutcome record={{}} trailer="never shown" />);
    expect(container.textContent).toBe("");
  });

  // Records written before the status field existed carry only a message:
  // judging them by the status alone would silently turn an old failure green.
  it("still judges a message-only record as failed", () => {
    expect(runOutcomeFailed({ last_run_error: "boom" })).toBe(true);
    expect(runOutcomeFailed({ last_run_error_code: "FUTURE_CODE" })).toBe(true);
    render(<LastRunOutcome record={{ last_run_error_code: "FUTURE_CODE", last_run_error: "new reason" }} />);
    expect(screen.getByText("Last run: failed")).toBeTruthy();
    expect(screen.getByText("FUTURE_CODE")).toBeTruthy();
  });
});
