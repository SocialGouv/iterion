// @vitest-environment jsdom

import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import VeilleBanner, { stopLabel, subjectLabel } from "./VeilleBanner";

describe("VeilleBanner", () => {
  it("names both channels so the operator knows what one click cuts", () => {
    // Stopping only the run watches is not idempotent: the card subscription
    // re-arms a fresh one at the next dispatch. The label has to say so.
    expect(stopLabel(["native:a"], ["run-1"])).toBe(
      "Stop watching (card and run)",
    );
    expect(stopLabel(["native:a"], [])).toBe("Stop watching");
    expect(subjectLabel(["native:a", "native:b"], ["run-1"])).toBe(
      "2 cards and 1 run tree",
    );
  });

  it("keeps the stop control reachable and reports a partial failure", () => {
    const onStop = vi.fn();
    render(
      <VeilleBanner
        issueIds={["native:a"]}
        runTargets={[]}
        busy={false}
        error="one watch could not be stopped"
        onStop={onStop}
        assistantLabel="Copi"
      />,
    );
    expect(screen.getByText(/Copi is standing by on 1 card/)).toBeTruthy();
    expect(screen.getByText("one watch could not be stopped")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Stop watching" }));
    expect(onStop).toHaveBeenCalledOnce();
  });

  it("shows activity instead of standby while the assistant is running", () => {
    const onStop = vi.fn();
    const { container } = render(
      <VeilleBanner
        issueIds={[]}
        runTargets={["run-1"]}
        busy={false}
        error={null}
        onStop={onStop}
        assistantLabel="Copi"
        isRunning
      />,
    );

    expect(screen.getByRole("status").textContent).toBe(
      "Copi is working on your request while watching 1 run tree.",
    );
    expect(container.querySelector(".animate-spin")).toBeTruthy();
    expect(container.textContent).not.toMatch(/standing by/);
    const stopButton = container.querySelector("button");
    expect(stopButton).toBeTruthy();
    fireEvent.click(stopButton!);
    expect(onStop).toHaveBeenCalledOnce();
  });
});
