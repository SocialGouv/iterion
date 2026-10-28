// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import TriggerList from "./TriggerList";
import type { TriggerSubscription } from "@/api/triggers";

// The edit dialog pulls in the bot catalog + auth; the row under test is
// independent of it.
vi.mock("./NewTriggerDialog", () => ({ default: () => null }));

afterEach(cleanup);

function sub(fields: Partial<TriggerSubscription>): TriggerSubscription {
  return {
    id: "t1",
    bot_id: "feature-dev",
    invocation: "board",
    match: {},
    enabled: true,
    ...fields,
  } as TriggerSubscription;
}

// A trigger subscription reported only the DECISION: that it fired, and
// whether the launch itself was refused. A trigger that fired and whose run
// died read exactly like one that fired and worked (#1501).
describe("TriggerList last-run outcome", () => {
  it("renders the launched run's outcome on the row", () => {
    render(
      <TriggerList
        subs={[
          sub({
            last_run_id: "run/9",
            last_run_status: "failed",
            last_run_error_code: "BUDGET_EXCEEDED",
            last_run_error: "max_cost_usd reached",
          }),
        ]}
        onChanged={() => {}}
      />,
    );
    expect(screen.getByText("Last run: failed")).toBeTruthy();
    expect(screen.getByText("BUDGET_EXCEEDED")).toBeTruthy();
    expect(screen.getByText("max_cost_usd reached")).toBeTruthy();
    // The consequence is the trigger's own, not the schedule's wording.
    expect(screen.getByText(/trigger stays armed/i)).toBeTruthy();
    expect(screen.getByRole("link", { name: "View run" }).getAttribute("href")).toBe("/runs/run%2F9");
  });

  it("says nothing about a trigger that never launched a run", () => {
    render(<TriggerList subs={[sub({})]} onChanged={() => {}} />);
    expect(screen.queryByText(/Last run:/)).toBeNull();
    expect(screen.queryByText(/trigger stays armed/i)).toBeNull();
  });

  it("carries a launch refusal beside the previous run's outcome", () => {
    render(
      <TriggerList
        subs={[sub({ enabled: false, last_run_status: "finished", last_error: "the org launch gate denied the run" })]}
        onChanged={() => {}}
      />,
    );
    expect(screen.getByText("Last run: finished")).toBeTruthy();
    expect(screen.getByText("Last launch refused: the org launch gate denied the run")).toBeTruthy();
    expect(screen.getByText("Trigger disabled.")).toBeTruthy();
  });
});
