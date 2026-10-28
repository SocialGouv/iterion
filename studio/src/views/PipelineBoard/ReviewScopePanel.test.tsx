// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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
    getReviewScope.mockResolvedValue({
      run_id: "run-1",
      gate_seq: 1,
      base_ref: "abc123",
      head_ref: "def456",
      available: true,
      groups: [
        {
          kind: "modified",
          files: [
            { path: "pkg/server/auth.go", status: "modified", additions: 4, deletions: 1 },
            { path: "pkg/server/routes.go", status: "modified", additions: 2, deletions: 0 },
          ],
        },
        {
          kind: "added",
          files: [
            { path: "docs/adr/0144-x.md", status: "added", additions: 30, deletions: 0 },
            { path: "studio/src/new.ts", status: "added", additions: 7, deletions: 0 },
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
    // line and nothing else. Open it, and prove the rows are now in scope
    // before asserting they are clean.
    const details = container.querySelector("details");
    expect(details).not.toBeNull();
    details?.setAttribute("open", "");
    expect(within(container).getByRole("button", { name: /pkg\/server\/auth\.go/ })).toBeTruthy();
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
