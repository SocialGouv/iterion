// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunSummary } from "@/api/runs";
import SubRunTabs, { MAIN_FLOW_TAB, statusDotClass } from "./SubRunTabs";

function child(partial: Partial<RunSummary>): RunSummary {
  return {
    id: "child-1",
    workflow_name: "episode",
    status: "running",
    created_at: "2026-07-15T10:00:00Z",
    updated_at: "2026-07-15T10:00:00Z",
    active: true,
    parent_run_id: "parent-1",
    ...partial,
  };
}

const THREE_CHILDREN: RunSummary[] = [
  child({ id: "c-run", status: "running", shard_label: "ep-1" }),
  child({ id: "c-paused", status: "paused_waiting_human", name: "brave-otter" }),
  child({ id: "c-done", status: "finished" }),
];

const NODE_BY_CHILD = new Map<string, string>([
  ["c-run", "produce_episode"],
  ["c-paused", "produce_episode"],
  ["c-done", "produce_episode"],
]);

function renderTabs(active: string, onSelect = vi.fn()) {
  render(
    <SubRunTabs
      mainLabel="pipeline-board-demo"
      childRuns={THREE_CHILDREN}
      nodeIdByChildId={NODE_BY_CHILD}
      active={active}
      onSelect={onSelect}
    />,
  );
  return onSelect;
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("SubRunTabs", () => {
  it("renders the Main tab plus one tab per child with its label", () => {
    renderTabs(MAIN_FLOW_TAB);
    const tabs = screen.getAllByRole("tab");
    expect(tabs.length).toBe(4);
    expect(tabs[0]?.textContent).toContain("Main");
    expect(tabs[0]?.textContent).toContain("pipeline-board-demo");
    // shard_label > name > "workflow #n" precedence.
    expect(tabs[1]?.textContent).toContain("ep-1");
    expect(tabs[2]?.textContent).toContain("brave-otter");
    expect(tabs[3]?.textContent).toContain("episode #3");
  });

  it("marks only the active tab aria-selected", () => {
    renderTabs("c-paused");
    const tabs = screen.getAllByRole("tab");
    expect(tabs.map((t) => t.getAttribute("aria-selected"))).toEqual([
      "false",
      "false",
      "true",
      "false",
    ]);
  });

  it("colors status dots per child status (running pulses)", () => {
    const { container } = render(
      <SubRunTabs
        mainLabel="demo"
        childRuns={THREE_CHILDREN}
        nodeIdByChildId={NODE_BY_CHILD}
        active={MAIN_FLOW_TAB}
        onSelect={vi.fn()}
      />,
    );
    const html = container.innerHTML;
    expect(html).toContain("bg-info animate-pulse"); // running
    expect(html).toContain("bg-warning"); // paused_waiting_human
    expect(html).toContain("bg-success"); // finished
  });

  it("invokes onSelect with the clicked tab's id", () => {
    const onSelect = renderTabs(MAIN_FLOW_TAB);
    fireEvent.click(screen.getAllByRole("tab")[2]!);
    expect(onSelect).toHaveBeenCalledWith("c-paused");
    fireEvent.click(screen.getAllByRole("tab")[0]!);
    expect(onSelect).toHaveBeenCalledWith(MAIN_FLOW_TAB);
  });

  it("implements the roving-tabindex keyboard pattern (WAI-ARIA tabs)", () => {
    const onSelect = renderTabs("c-run");
    const tabs = screen.getAllByRole("tab");
    // Only the active tab sits in the Tab order.
    expect(tabs.map((t) => t.tabIndex)).toEqual([-1, 0, -1, -1]);
    // ArrowRight from the active tab focuses + selects the next one.
    tabs[1]!.focus();
    fireEvent.keyDown(screen.getByRole("tablist"), { key: "ArrowRight" });
    expect(onSelect).toHaveBeenCalledWith("c-paused");
    expect(document.activeElement).toBe(tabs[2]);
    // Home selects the Main tab; ArrowLeft from Main wraps to the end.
    fireEvent.keyDown(screen.getByRole("tablist"), { key: "Home" });
    expect(onSelect).toHaveBeenCalledWith(MAIN_FLOW_TAB);
    expect(document.activeElement).toBe(tabs[0]);
    fireEvent.keyDown(screen.getByRole("tablist"), { key: "ArrowLeft" });
    expect(onSelect).toHaveBeenCalledWith("c-done");
  });

  it("tooltips carry run id, status, and the spawning node", () => {
    renderTabs(MAIN_FLOW_TAB);
    const paused = screen.getAllByRole("tab")[2]!;
    const title = paused.getAttribute("title") ?? "";
    expect(title).toContain("c-paused");
    expect(title).toContain("Paused — input needed");
    expect(title).toContain("spawned by produce_episode");
  });
});

describe("statusDotClass", () => {
  it("derives solid dot classes from the tone map", () => {
    expect(statusDotClass("running")).toBe("bg-info animate-pulse");
    expect(statusDotClass("failed")).toBe("bg-danger");
    expect(statusDotClass("queued")).toBe("bg-fg-subtle");
  });
});
