// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunHeader, RunSummary } from "@/api/runs";
import RunChildrenPanel from "./RunChildrenPanel";

// Mock the children fetcher — the panel's only data dependency.
const getRunChildren = vi.fn();
vi.mock("@/api/runs", () => ({
  getRunChildren: (...a: unknown[]) => getRunChildren(...a),
}));

function parent(id = "parent-1"): RunHeader {
  return {
    id,
    workflow_name: "fan-out",
    status: "running",
    created_at: "2026-07-13T10:00:00Z",
    updated_at: "2026-07-13T10:00:00Z",
    active_duration_ms: 0,
  } as RunHeader;
}

function renderPanel(run: RunHeader) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RunChildrenPanel run={run} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("RunChildrenPanel", () => {
  it("renders one linked row per shard child with its shard label", async () => {
    const children: RunSummary[] = [
      {
        id: "child-aaaaaaaa",
        workflow_name: "fan-out",
        status: "finished",
        created_at: "2026-07-13T10:01:00Z",
        updated_at: "2026-07-13T10:05:00Z",
        active: false,
        source_kind: "shard",
        parent_run_id: "parent-1",
        // shard_index 0 is omitted on the wire (omitempty) — the label
        // must still render "#1/2".
        shard_count: 2,
      },
      {
        id: "child-bbbbbbbb",
        workflow_name: "fan-out",
        status: "running",
        created_at: "2026-07-13T10:01:00Z",
        updated_at: "2026-07-13T10:05:00Z",
        active: true,
        source_kind: "shard",
        parent_run_id: "parent-1",
        shard_index: 1,
        shard_count: 2,
        shard_label: "linux/arm64",
      },
    ];
    getRunChildren.mockResolvedValue(children);

    renderPanel(parent());

    await waitFor(() => expect(screen.getByText(/Children \(2\)/)).toBeTruthy());

    // Both children link to their run console.
    const links = screen
      .getAllByRole("link")
      .filter((el) => el.getAttribute("href")?.startsWith("/runs/"));
    expect(links.length).toBe(2);
    expect(links[0]?.getAttribute("href")).toContain("child-aaaaaaaa");
    expect(links[1]?.getAttribute("href")).toContain("child-bbbbbbbb");

    // Labels: derived "#1/2" for the omitted-index shard, explicit label
    // for the second.
    expect(screen.getByText("#1/2")).toBeTruthy();
    expect(screen.getByText("linux/arm64")).toBeTruthy();
  });

  it("renders nothing when the run has no children", async () => {
    getRunChildren.mockResolvedValue([]);
    const { container } = renderPanel(parent());
    // Nothing to render (empty children); the panel stays absent.
    await waitFor(() => expect(getRunChildren).toHaveBeenCalled());
    expect(container.querySelector("ul")).toBeNull();
    expect(screen.queryByText(/Children/)).toBeNull();
  });
});
