// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { RunFiles } from "@/api/runs";

// Mock the data hook so we can drive FilesPanel purely from a fixed
// RunFiles payload — the empty-state copy is what we're locking here.
const mockUseRunFiles = vi.fn();
vi.mock("@/hooks/useRunFiles", () => ({
  useRunFiles: (...args: unknown[]) => mockUseRunFiles(...args),
}));

import FilesPanel from "./FilesPanel";

afterEach(() => {
  cleanup();
  mockUseRunFiles.mockReset();
});

function setData(data: RunFiles) {
  mockUseRunFiles.mockReturnValue({
    data,
    loading: false,
    error: null,
    refresh: () => {},
  });
}

describe("FilesPanel empty states", () => {
  it("shows a 'building' hint for a live run whose files aren't recorded yet", () => {
    setData({ files: [], available: false, reason: "building" });
    render(<FilesPanel runId="r1" onSelectFile={() => {}} />);
    expect(
      screen.getByText(/becomes available when the run finishes/i),
    ).toBeTruthy();
    // Must NOT read as an error / broken-repo state.
    expect(screen.queryByText(/not a git checkout/i)).toBeNull();
  });

  it("still distinguishes a genuinely-removed worktree from a building run", () => {
    setData({ files: [], available: false, reason: "not_git_repo" });
    render(<FilesPanel runId="r2" onSelectFile={() => {}} />);
    expect(screen.getByText(/no longer exists/i)).toBeTruthy();
  });
});
