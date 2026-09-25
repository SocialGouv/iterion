// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { ScheduledBot } from "@/api/schedules";
import ScheduleOutcome from "./ScheduleOutcome";

afterEach(cleanup);

function schedule(fields: Partial<ScheduledBot>): ScheduledBot {
  return { id: "s1", ...fields } as ScheduledBot;
}

describe("ScheduleOutcome", () => {
  it("shows the typed terminal failure, the run link, and the unchanged cadence", () => {
    render(<ScheduleOutcome schedule={schedule({ last_run_id: "run/1", last_run_status: "failed", last_run_error_code: "SANDBOX_DRIVER_UNAVAILABLE", last_run_error: "sandbox start: no runtime" })} />);
    expect(screen.getByText("SANDBOX_DRIVER_UNAVAILABLE")).toBeTruthy();
    expect(screen.getByText(/install Docker or Podman/)).toBeTruthy();
    expect(screen.getByText(/Kubernetes configuration and access in cloud/)).toBeTruthy();
    expect(screen.getByText(/keeps its cadence/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "View run" }).getAttribute("href")).toBe("/runs/run%2F1");
  });

  it("keeps an unknown code and raw message visible without guessing their cause", () => {
    render(<ScheduleOutcome schedule={schedule({ disabled: true, last_run_error_code: "FUTURE_CODE", last_run_error: "new reason" })} />);
    expect(screen.getByText("FUTURE_CODE")).toBeTruthy();
    expect(screen.getByText("new reason")).toBeTruthy();
    expect(screen.getByText("Schedule paused.")).toBeTruthy();
    expect(screen.queryByText(/install Docker or Podman/)).toBeNull();
  });

  it("shows a launch refusal separately from the preceding successful run", () => {
    render(<ScheduleOutcome schedule={schedule({ last_run_status: "finished", last_error: "quota reached" })} />);
    expect(screen.getByText("Last run: finished")).toBeTruthy();
    expect(screen.getByText("Last launch refused: quota reached")).toBeTruthy();
  });

  it("removes the failure advice after a successful outcome clears the error", () => {
    const { rerender } = render(<ScheduleOutcome schedule={schedule({ last_run_status: "failed", last_run_error_code: "SANDBOX_DRIVER_UNAVAILABLE" })} />);
    expect(screen.getByText(/install Docker or Podman/)).toBeTruthy();
    expect(screen.getByText(/Kubernetes configuration and access in cloud/)).toBeTruthy();
    rerender(<ScheduleOutcome schedule={schedule({ last_run_status: "finished" })} />);
    expect(screen.getByText("Last run: finished")).toBeTruthy();
    expect(screen.queryByText(/install Docker or Podman/)).toBeNull();
    expect(screen.queryByText(/keeps its cadence/)).toBeNull();
  });

  it("stays empty before the first outcome", () => {
    const { container } = render(<ScheduleOutcome schedule={schedule({})} />);
    expect(container.textContent).toBe("");
  });
});
