// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { downloadArtifactFile, type ArtifactFile } from "@/api/runs";
import DeliverablesRow from "./DeliverablesRow";

vi.mock("@/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/runs")>();
  return {
    ...actual,
    listArtifactFiles: vi.fn(async () => []),
    downloadArtifactFile: vi.fn(async () => ({
      cancelled: false,
      contentType: "application/zip",
      localPath: null,
    })),
  };
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const files: ArtifactFile[] = [
  { path: "rgaa-2026-10-22.md", size: 50533, modified_at: "2026-10-22T11:37:00Z" },
  { path: "out/bundle.zip", size: 53366, modified_at: "2026-10-22T11:37:00Z" },
];

function mount(seed: ArtifactFile[], path = "/runs/run-1") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["run-artifact-files", "run-1"], seed);
  const loc = memoryLocation({ path, record: true });
  render(
    <QueryClientProvider client={client}>
      <Router hook={loc.hook} searchHook={loc.searchHook}>
        <DeliverablesRow runId="run-1" />
      </Router>
    </QueryClientProvider>,
  );
  return loc;
}

describe("DeliverablesRow", () => {
  it("renders nothing while the run left no file", () => {
    mount([]);
    expect(screen.queryByRole("group", { name: "Run deliverables" })).toBeNull();
  });

  it("links each file to its preview on the run page", () => {
    mount(files);
    const row = screen.getByRole("group", { name: "Run deliverables" });
    expect(row.textContent).toContain("Deliverables");
    expect(screen.getByRole("link", { name: "rgaa-2026-10-22.md" }).getAttribute("href")).toBe(
      "/runs/run-1?file=rgaa-2026-10-22.md",
    );
    expect(screen.getByRole("link", { name: "out/bundle.zip" }).getAttribute("href")).toBe(
      "/runs/run-1?file=out%2Fbundle.zip",
    );
  });

  it("keeps the run's other query keys in the preview link (a foreign-store run)", () => {
    mount(files, "/runs/run-1?store=%2Fsrv%2Fstore");
    expect(screen.getByRole("link", { name: "rgaa-2026-10-22.md" }).getAttribute("href")).toBe(
      "/runs/run-1?store=%2Fsrv%2Fstore&file=rgaa-2026-10-22.md",
    );
  });

  it("downloads through the shared gesture, not a bare anchor", async () => {
    mount(files);
    fireEvent.click(screen.getByRole("button", { name: "Download out/bundle.zip" }));
    await waitFor(() => expect(downloadArtifactFile).toHaveBeenCalledWith("run-1", "out/bundle.zip"));
    expect(screen.queryByRole("link", { name: "Download out/bundle.zip" })).toBeNull();
  });
});
