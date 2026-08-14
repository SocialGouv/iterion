// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const getReviewScope = vi.hoisted(() => vi.fn());

vi.mock("@/api/runs", () => ({
  getReviewScope: (...args: unknown[]) => getReviewScope(...args),
  workspaceFileURL: () => "",
}));

vi.mock("@/components/Runs/FileDiffDialog", () => ({
  default: () => null,
}));

vi.mock("./ImagePreview", () => ({
  ImagePreviewDialog: () => null,
}));

import { ReviewScopePanel } from "./ReviewScopePanel";

function client() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function wrap(ui: React.ReactElement, qc: QueryClient) {
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>;
}

afterEach(() => {
  cleanup();
  getReviewScope.mockReset();
});

describe("ReviewScopePanel", () => {
  it("renders the unavailable reason instead of a silent empty panel", async () => {
    getReviewScope.mockResolvedValue({
      run_id: "run-1",
      gate_seq: 1,
      base_ref: "",
      head_ref: "",
      available: false,
      reason: "this run captured no workspace snapshots at all",
      groups: [],
      total_files: 0,
    });
    render(wrap(<ReviewScopePanel runId="run-1" pauseKey="k1" />, client()));
    await waitFor(() => {
      expect(screen.getByRole("status").textContent).toContain(
        "this run captured no workspace snapshots at all",
      );
    });
  });

  it("does not reuse a cached scope when pauseKey changes on the same run", async () => {
    getReviewScope
      .mockResolvedValueOnce({
        run_id: "run-1",
        gate_seq: 1,
        base_ref: "",
        head_ref: "",
        available: true,
        groups: [],
        total_files: 0,
      })
      .mockResolvedValueOnce({
        run_id: "run-1",
        gate_seq: 2,
        base_ref: "",
        head_ref: "",
        available: true,
        groups: [],
        total_files: 0,
      });
    const qc = client();
    const view = render(wrap(<ReviewScopePanel runId="run-1" pauseKey="gate-1" />, qc));
    await waitFor(() => {
      expect(screen.getByText(/Nothing changed in the workspace/)).toBeTruthy();
    });
    expect(getReviewScope).toHaveBeenCalledTimes(1);

    view.rerender(wrap(<ReviewScopePanel runId="run-1" pauseKey="gate-1" />, qc));
    expect(getReviewScope).toHaveBeenCalledTimes(1);

    view.rerender(wrap(<ReviewScopePanel runId="run-1" pauseKey="gate-2" />, qc));
    await waitFor(() => {
      expect(getReviewScope).toHaveBeenCalledTimes(2);
    });
  });
});
