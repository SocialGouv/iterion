// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

import { LastRunSection } from "./LastRunSection";

// Mock the runs API: getRun feeds paused-run detection, getRunWorkflow feeds
// the paused node's OUTPUT schema (what HumanPromptForm renders), resumeRun is
// the answer call, getRunChildren feeds the per-row children disclosure.
const getRun = vi.fn();
const getRunWorkflow = vi.fn();
const resumeRun = vi.fn().mockResolvedValue({ run_id: "r1", status: "running" });
const getRunChildren = vi.fn().mockResolvedValue([]);
vi.mock("@/api/runs", () => ({
  getRun: (...a: unknown[]) => getRun(...a),
  getRunWorkflow: (...a: unknown[]) => getRunWorkflow(...a),
  resumeRun: (...a: unknown[]) => resumeRun(...a),
  getRunChildren: (...a: unknown[]) => getRunChildren(...a),
}));

// PauseForm's editor-buffer source; the board caller overrides it.
vi.mock("@/store/document", () => ({
  useDocumentStore: (sel: (s: { currentSource: string | null }) => unknown) =>
    sel({ currentSource: "some-other-bot.bot" }),
}));

// HumanPromptForm reads run-store selectors; on the board (onResumed set) they
// must not fire, but the hooks are still called — return no-ops.
vi.mock("@/store/run", () => ({
  useRunStore: (sel: (s: Record<string, unknown>) => unknown) =>
    sel({
      setRunStatus: vi.fn(),
      requestWsReconnect: vi.fn(),
      applySnapshot: vi.fn(),
      resyncEventsAfterResume: vi.fn(),
    }),
}));

vi.mock("@/components/Runs/BranchDiffModal", () => ({ default: () => null }));

function renderSection(runID: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LastRunSection runID={runID} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("LastRunSection answer-from-board affordance", () => {
  it("renders the paused human node's OUTPUT-schema fields, not the checkpoint context vars", async () => {
    getRun.mockResolvedValue({
      run: {
        id: "r1",
        status: "paused_waiting_human",
        checkpoint: {
          node_id: "approval",
          interaction_id: "r1_approval",
          // Context vars carried on the checkpoint — must NOT become the form.
          interaction_questions: { change_summary: "bump sdk", service: "checkout-api" },
        },
      },
      executions: [],
      last_seq: 0,
    });
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "approval",
          output_schema: [
            { name: "environment", type: "string", enum_values: ["staging", "production"] },
            { name: "approve", type: "bool" },
            { name: "reviewer", type: "string" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: false,
    });

    renderSection("r1");

    await waitFor(() => expect(screen.getByText(/Awaiting input/i)).toBeTruthy());
    // The schema-driven wizard renders the first output-schema field
    // (environment, an enum) with its options — proof the form comes from
    // the node's output schema, not the checkpoint's plain context-var map.
    await waitFor(() => expect(screen.getByText(/environment/i)).toBeTruthy());
    expect(screen.getByText(/staging/i)).toBeTruthy();
    expect(screen.getByText(/production/i)).toBeTruthy();
    // The checkpoint context vars must NOT be rendered as answer fields…
    expect(screen.queryByLabelText(/change_summary/i)).toBeNull();
    expect(screen.queryByLabelText(/service/i)).toBeNull();
    // …but they ARE shown, read-only, as the review context above the
    // form: they are exactly what the operator is validating (iterion#332).
    const context = document.querySelector("section[aria-label='Review context']");
    expect(context).toBeTruthy();
    expect(context?.textContent).toContain("checkout-api");
    expect(context?.textContent).toContain("bump sdk");
  });

  it("shows only the plain last-run panel when the run is not paused", async () => {
    getRun.mockResolvedValue({
      run: { id: "r1", status: "finished", checkpoint: {} },
      executions: [],
      last_seq: 0,
    });
    renderSection("r1");
    await waitFor(() => expect(screen.getByText(/Last run/i)).toBeTruthy());
    expect(screen.queryByText(/Awaiting input/i)).toBeNull();
  });
});

describe("LastRunSection children disclosure (lazy)", () => {
  it("fetches children only on expand, then lists them with links", async () => {
    getRun.mockResolvedValue({
      run: { id: "r1", status: "finished", checkpoint: {} },
      executions: [],
      last_seq: 0,
    });
    getRunChildren.mockResolvedValue([
      {
        id: "child-aaaaaaaa",
        workflow_name: "fan-out",
        status: "finished",
        created_at: "2026-07-13T10:01:00Z",
        updated_at: "2026-07-13T10:05:00Z",
        active: false,
        shard_count: 2,
      },
    ]);

    renderSection("r1");
    // Collapsed by default → no children request (guards against N+1).
    await waitFor(() => expect(screen.getByText(/▸ Children/)).toBeTruthy());
    expect(getRunChildren).not.toHaveBeenCalled();

    // Expanding fires the single fetch and lists the child.
    fireEvent.click(screen.getByText(/▸ Children/));
    await waitFor(() => expect(getRunChildren).toHaveBeenCalledWith("r1"));
    await waitFor(() => expect(screen.getByText("#1/2")).toBeTruthy());
    const link = screen
      .getAllByRole("link")
      .find((el) => el.getAttribute("href")?.includes("child-aaaaaaaa"));
    expect(link).toBeTruthy();
  });
});

describe("LastRunSection run history list", () => {
  function renderHistory(runs: { run_id: string; workdir?: string; at: string }[]) {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    return render(
      <QueryClientProvider client={qc}>
        <LastRunSection runs={runs} />
      </QueryClientProvider>,
    );
  }

  it("renders one panel per run, newest-last, with a link to each run", async () => {
    getRun.mockResolvedValue({
      run: { id: "x", status: "finished", checkpoint: {} },
      executions: [],
      last_seq: 0,
    });
    renderHistory([
      { run_id: "run-aaaaaaaa", workdir: "/tmp/wd-1", at: "2026-07-13T10:00:00Z" },
      { run_id: "run-bbbbbbbb", workdir: "/tmp/wd-2", at: "2026-07-13T11:00:00Z" },
    ]);
    // Header switches to "Run history" when there is more than one run.
    await waitFor(() => expect(screen.getByText(/Run history/i)).toBeTruthy());
    // Both runs are linked to their run console (order preserved: newest-last).
    const runLinks = screen
      .getAllByRole("link")
      .filter((el) => el.getAttribute("href")?.startsWith("/runs/"));
    expect(runLinks.length).toBe(2);
    expect(runLinks[0]?.getAttribute("href")).toContain("run-aaaaaaaa");
    expect(runLinks[1]?.getAttribute("href")).toContain("run-bbbbbbbb");
    // Both worktrees surface.
    expect(screen.getByText("/tmp/wd-1")).toBeTruthy();
    expect(screen.getByText("/tmp/wd-2")).toBeTruthy();
  });

  it("keeps the awaiting-input affordance working for a paused run in the list", async () => {
    getRun.mockImplementation((id: string) =>
      Promise.resolve({
        run: {
          id,
          status: id === "run-paused0" ? "paused_waiting_human" : "finished",
          checkpoint:
            id === "run-paused0"
              ? { node_id: "approval", interaction_id: "run-paused0_approval" }
              : {},
        },
        executions: [],
        last_seq: 0,
      }),
    );
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "approval",
          output_schema: [
            { name: "environment", type: "string", enum_values: ["staging", "production"] },
          ],
        },
      ],
      stale_hash: false,
    });
    renderHistory([
      { run_id: "run-done0000", at: "2026-07-13T10:00:00Z" },
      { run_id: "run-paused0", at: "2026-07-13T11:00:00Z" },
    ]);
    // The paused run in the list still surfaces its schema-driven answer form
    // (the node's output-schema enum options render, proving the affordance
    // is live for a paused run nested inside the history list).
    await waitFor(() =>
      expect(screen.getAllByText(/Awaiting input/i).length).toBeGreaterThan(0),
    );
    await waitFor(() =>
      expect(screen.getAllByText(/staging/i).length).toBeGreaterThan(0),
    );
    expect(screen.getAllByText(/production/i).length).toBeGreaterThan(0);
  });
});
