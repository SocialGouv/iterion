// @vitest-environment jsdom
//
// The dispatcher's running table repeats one "Cancel" per in-flight run, and
// axe has no rule for two controls answering to one name (#2591). Mounted
// with the table's own props rather than the whole dashboard: the view is
// WebSocket-driven, and a snapshot is exactly what the table consumes.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { RunningView } from "@/api/dispatcher";
import {
  expectNoViolationsStrict,
  expectUniqueAccessibleNames,
  setupMatchMedia,
} from "@/__tests__/a11y/axeHelpers";

import RunningTable from "./RunningTable";

setupMatchMedia();

// The stall decoration reads Date.now() against last_event_at, so the row's
// class, its title and the ⏱ marker would otherwise be decided by the
// machine's clock. Only Date is faked — the timers React and
// testing-library schedule on stay real.
const NOW = "2026-11-03T02:00:00Z";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
});
afterEach(() => {
  vi.useRealTimers();
  cleanup();
});

const ROWS: RunningView[] = [
  {
    issue_id: "i-1",
    identifier: "SocialGouv/iterion#2591",
    run_id: "0199aaaa-1111-7000-8000-000000000001",
    workflow_state: "running",
    workspace_path: "/home/runner/.iterion/worktrees/wf-2591",
    started_at: "2026-11-03T01:50:00Z",
    // 10 s ago against a 600 s budget: no stall decoration.
    last_event_at: "2026-11-03T01:59:50Z",
    last_event_name: "tool",
  },
  {
    issue_id: "i-2",
    identifier: "SocialGouv/iterion#2594",
    run_id: "0199bbbb-2222-7000-8000-000000000002",
    workflow_state: "running",
    started_at: "2026-11-03T01:40:00Z",
    // 19 min ago: past the whole budget, so this row is the stalled one.
    last_event_at: "2026-11-03T01:41:00Z",
    attempt: 1,
  },
];

function mountRunning(): HTMLElement {
  return render(
    <main>
      <RunningTable
        rows={ROWS}
        stallTimeoutS={600}
        onCancel={() => {}}
        onOpenRun={() => {}}
      />
    </main>,
  ).container;
}

describe("dispatcher running table naming (RGAA 11.1)", () => {
  it("names every row's Cancel after the run it cancels", () => {
    mountRunning();
    // Both stall states are on screen, so the entries below judge the
    // decorated row as well as the quiet one.
    expect(screen.getAllByText("⏱")).toHaveLength(1);
    for (const name of ["Cancel SocialGouv/iterion#2591", "Cancel SocialGouv/iterion#2594"]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
  });

  it("keeps the visible word first, so the name contains the label (SC 2.5.3)", () => {
    mountRunning();
    expect(
      screen.getByRole("button", { name: "Cancel SocialGouv/iterion#2591" }).textContent,
    ).toBe("Cancel");
  });

  it("leaves no two row controls sharing an accessible name", () => {
    const container = mountRunning();
    // Both row-specific controls: the Cancel action and the run-id link
    // that opens the run. Nothing else in the table is interactive.
    expectUniqueAccessibleNames(container, "Dispatcher · running rows", {
      roles: ["button"],
      count: 4,
    });
  });
});

describe("dispatcher running table axe", () => {
  it("RunningTable — rows in flight, axe smoke, incompletes included", async () => {
    const container = mountRunning();
    expect(screen.getByRole("table", { name: /in flight/ })).toBeTruthy();
    await expectNoViolationsStrict(container, "Dispatcher RunningTable");
  });
});
