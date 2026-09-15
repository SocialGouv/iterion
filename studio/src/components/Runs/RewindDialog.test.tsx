// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunHeader } from "@/api/runs";

const mocks = vi.hoisted(() => ({
  rewindRun: vi.fn(),
  getRun: vi.fn(),
  applySnapshot: vi.fn(),
  addToast: vi.fn(),
}));

vi.mock("@/api/runs", () => ({
  rewindRun: (...args: unknown[]) => mocks.rewindRun(...args),
  getRun: (...args: unknown[]) => mocks.getRun(...args),
}));

vi.mock("@/store/run", () => ({
  useRunStore: (select: (state: Record<string, unknown>) => unknown) =>
    select({ applySnapshot: mocks.applySnapshot }),
}));

vi.mock("@/store/ui", () => ({
  useUIStore: (select: (state: Record<string, unknown>) => unknown) =>
    select({ addToast: mocks.addToast }),
}));

import RewindDialog from "./RewindDialog";

function failedRun(): RunHeader {
  return {
    id: "run-failed",
    workflow_name: "workflow",
    status: "failed",
    rewindable: true,
    created_at: "2026-08-30T09:00:00Z",
    updated_at: "2026-08-30T09:05:00Z",
    active_duration_ms: 1,
    checkpoint: {
      node_id: "fail",
      outputs: {
        prepare_planner: { status: "ok" },
        pin_repaired_surveys: { status: "failed" },
      },
    },
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("RewindDialog", () => {
  it("rewinds a terminal run without restoring files, then hands off to resume", async () => {
    const snapshot = { run: { ...failedRun(), status: "cancelled" } };
    mocks.rewindRun.mockResolvedValue({
      run_id: "run-failed",
      node_id: "prepare_planner",
      dropped_nodes: ["prepare_planner", "fail"],
      status: "cancelled",
    });
    mocks.getRun.mockResolvedValue(snapshot);
    const onOpenChange = vi.fn();
    const onRewound = vi.fn();

    render(
      <RewindDialog
        run={failedRun()}
        open
        onOpenChange={onOpenChange}
        onRewound={onRewound}
      />,
    );

    fireEvent.change(screen.getByRole("combobox", { name: /Recovery node/i }), {
      target: { value: "prepare_planner" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Rewind checkpoint" }));

    await waitFor(() =>
      expect(mocks.rewindRun).toHaveBeenCalledWith("run-failed", {
        node_id: "prepare_planner",
        restore_scope: "none",
      }),
    );
    expect(mocks.getRun).toHaveBeenCalledWith("run-failed");
    expect(mocks.applySnapshot).toHaveBeenCalledWith(snapshot);
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onRewound).toHaveBeenCalledTimes(1);
  });

  it("uses auto targeting only when the operator selects source recovery", async () => {
    mocks.rewindRun.mockResolvedValue({
      run_id: "run-failed",
      node_id: "prepare_planner",
      dropped_nodes: ["prepare_planner"],
      status: "cancelled",
    });
    mocks.getRun.mockResolvedValue({ run: { ...failedRun(), status: "cancelled" } });

    render(
      <RewindDialog
        run={failedRun()}
        open
        onOpenChange={vi.fn()}
        onRewound={vi.fn()}
      />,
    );

    fireEvent.click(
      screen.getByRole("checkbox", {
        name: /Target workflow source changes automatically/i,
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Rewind checkpoint" }));

    await waitFor(() =>
      expect(mocks.rewindRun).toHaveBeenCalledWith("run-failed", {
        auto: true,
        restore_scope: "none",
      }),
    );
  });
});
