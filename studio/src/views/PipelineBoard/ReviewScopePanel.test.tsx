// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

setupMatchMedia();

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

describe("a11y / ReviewScopePanel", () => {
  // Two file groups with two files each: the panel repeats a per-file row of
  // controls, and one row of one group cannot show whether their names say
  // which file they act on.
  it("has no violation over a populated scope", async () => {
    // The response's own shape (ReviewScopeGroup + RunFile), and BOTH header
    // branches: a group that records a node renders its id as code, and the
    // trailing group — the one that collects work from node kinds recording
    // no boundary — carries an empty node_id and renders its label instead.
    getReviewScope.mockResolvedValue({
      run_id: "run-1",
      gate_seq: 1,
      base_ref: "abc123",
      head_ref: "def456",
      available: true,
      groups: [
        {
          node_id: "implement",
          label: "Implement",
          iteration: 2,
          files: [
            { path: "pkg/server/auth.go", status: "M", added: 4, deleted: 1 },
            { path: "pkg/server/routes.go", status: "M", added: 2, deleted: 0 },
          ],
        },
        {
          node_id: "",
          label: "Other changes in range",
          files: [
            { path: "docs/adr/0144-x.md", status: "A", added: 30, deleted: 0 },
            { path: "studio/src/new.ts", status: "A", added: 7, deleted: 0 },
          ],
        },
      ],
      total_files: 4,
    });
    const { container } = render(
      wrap(<ReviewScopePanel runId="run-1" pauseKey="k1" />, client()),
    );
    await waitFor(() => {
      expect(screen.getByText("pkg/server/auth.go")).toBeTruthy();
    });
    // The groups live inside a COLLAPSED <details>, whose content axe treats
    // as hidden — auditing the panel as it arrives would judge the summary
    // line and nothing else. Opened the way the operator opens it (the
    // summary, not the attribute), so the state this entry audits is one a
    // click is proven to reach, and the rows are shown to be in scope before
    // they are asserted clean.
    const details = container.querySelector("details");
    const summary = container.querySelector("summary");
    expect(summary).not.toBeNull();
    fireEvent.click(summary as Element);
    expect(details?.open).toBe(true);
    expect(within(container).getByRole("button", { name: /pkg\/server\/auth\.go/ })).toBeTruthy();
    // Both header branches are in scope, not just the one this fixture
    // happens to list first.
    expect(within(container).getByText("implement")).toBeTruthy();
    expect(within(container).getByText("Other changes in range")).toBeTruthy();
    await expectNoViolationsStrict(container, "ReviewScopePanel");
  });

  it("has no violation on the unavailable-reason state", async () => {
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
    const { container } = render(
      wrap(<ReviewScopePanel runId="run-1" pauseKey="k1" />, client()),
    );
    await waitFor(() => {
      expect(screen.getByRole("status")).toBeTruthy();
    });
    await expectNoViolationsStrict(container, "ReviewScopePanel (unavailable)");
  });
});
