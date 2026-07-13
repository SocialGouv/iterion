// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

import { LastRunSection } from "./LastRunSection";

// Mock the runs API: getRun feeds paused-run detection, getRunWorkflow feeds
// the paused node's OUTPUT schema (what HumanPromptForm renders), resumeRun is
// the answer call.
const getRun = vi.fn();
const getRunWorkflow = vi.fn();
const resumeRun = vi.fn().mockResolvedValue({ run_id: "r1", status: "running" });
vi.mock("@/api/runs", () => ({
  getRun: (...a: unknown[]) => getRun(...a),
  getRunWorkflow: (...a: unknown[]) => getRunWorkflow(...a),
  resumeRun: (...a: unknown[]) => resumeRun(...a),
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
    // The checkpoint context vars must NOT be rendered as answer fields.
    expect(screen.queryByText(/change_summary/i)).toBeNull();
    expect(screen.queryByText(/checkout-api/i)).toBeNull();
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
