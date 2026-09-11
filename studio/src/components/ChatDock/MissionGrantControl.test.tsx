// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { missionGrantCovers } from "@/lib/chatDock/missionGrant";

import MissionGrantControl from "./MissionGrantControl";

const ASSISTANT = "copi-1";
const TARGET = "run-a";

describe("MissionGrantControl", () => {
  // No global setup file, so testing-library's auto-cleanup is not wired:
  // without this the previous test's DOM survives and role queries match twice.
  beforeEach(() => {
    cleanup();
    localStorage.clear();
  });

  it("delegates the whole grantable set, not just the verb on screen", () => {
    // A repair allowed to rewind but stopping to ask before resuming has not
    // been delegated — it has been half-delegated, and it stalls at step two.
    render(
      <MissionGrantControl
        assistantRunId={ASSISTANT}
        targetRunId={TARGET}
        action="run.rewind"
        assistantLabel="Copi"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /see this run through/i }));

    expect(missionGrantCovers(ASSISTANT, TARGET, "run.rewind")).toBe(true);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.resume")).toBe(true);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.watch")).toBe(true);
  });

  it("stays bounded to this run", () => {
    render(
      <MissionGrantControl
        assistantRunId={ASSISTANT}
        targetRunId={TARGET}
        action="run.resume"
        assistantLabel="Copi"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /see this run through/i }));
    expect(missionGrantCovers(ASSISTANT, "another-run", "run.resume")).toBe(false);
  });

  it("flips to a revoke affordance once granted, and revokes", () => {
    // The operator must be able to see that a delegation is live and end it
    // in one click — a standing authorisation you cannot find is one you
    // cannot withdraw.
    render(
      <MissionGrantControl
        assistantRunId={ASSISTANT}
        targetRunId={TARGET}
        action="run.resume"
        assistantLabel="Copi"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /see this run through/i }));
    const stop = screen.getByRole("button", { name: /stop delegating/i });
    fireEvent.click(stop);
    expect(missionGrantCovers(ASSISTANT, TARGET, "run.resume")).toBe(false);
    expect(screen.getByRole("button", { name: /see this run through/i })).toBeTruthy();
  });

  it("renders nothing without both ends of the scope", () => {
    // No assistant to bind to, or no run to bound by, and the control would
    // promise a scope it cannot enforce.
    const { container, rerender } = render(
      <MissionGrantControl
        assistantRunId={null}
        targetRunId={TARGET}
        action="run.resume"
        assistantLabel="Copi"
      />,
    );
    expect(container.textContent).toBe("");
    rerender(
      <MissionGrantControl
        assistantRunId={ASSISTANT}
        targetRunId={null}
        action="run.resume"
        assistantLabel="Copi"
      />,
    );
    expect(container.textContent).toBe("");
  });

  it("is not offered for a verb that writes outside the run", () => {
    const { container } = render(
      <MissionGrantControl
        assistantRunId={ASSISTANT}
        targetRunId={TARGET}
        action="editor.save"
        assistantLabel="Copi"
      />,
    );
    expect(container.textContent).toBe("");
  });
});
