// @vitest-environment jsdom
// The `?file=` address of an artifact file: who answers it, when it is
// honoured again, and what the URL keeps when a preview opens or closes.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { fetchArtifactFile, listArtifactFiles, type ArtifactFile } from "@/api/runs";
import ArtifactFilesPanel from "./ArtifactFilesPanel";

vi.mock("@/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/runs")>();
  return {
    ...actual,
    listArtifactFiles: vi.fn(async () => []),
    fetchArtifactFile: vi.fn(async () => ({
      blob: { text: async () => "# Rapport" } as unknown as Blob,
      contentType: "text/markdown; charset=utf-8",
    })),
    downloadArtifactFile: vi.fn(),
  };
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const report: ArtifactFile = {
  path: "report.md",
  size: 12,
  modified_at: "2026-10-22T11:37:00Z",
};

// Two run tabs stay mounted at once in the studio (RunsTabsView hides
// the inactive one); both list a file of the same name.
function mount(path: string, runIds: string[]) {
  vi.mocked(listArtifactFiles).mockImplementation(async () => [report]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const loc = memoryLocation({ path, record: true });
  render(
    <QueryClientProvider client={client}>
      <Router hook={loc.hook} searchHook={loc.searchHook}>
        <Route path="/runs/:id">
          {runIds.map((id) => (
            <ArtifactFilesPanel key={id} runId={id} />
          ))}
        </Route>
      </Router>
    </QueryClientProvider>,
  );
  return loc;
}

describe("ArtifactFilesPanel ?file= deep link", () => {
  it("opens the requested file for the routed run only, never for a hidden run tab", async () => {
    mount("/runs/A?file=report.md", ["A", "B"]);
    // Both panels list their files before any verdict: a hidden tab's
    // late open would otherwise slip past a count taken at the first call.
    await waitFor(() => expect(listArtifactFiles).toHaveBeenCalledTimes(2));
    expect(await screen.findByRole("heading", { name: "Rapport" })).toBeTruthy();
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20));
    });
    expect(vi.mocked(fetchArtifactFile).mock.calls.map((c) => [c[0], c[1]])).toEqual([
      ["A", "report.md"],
    ]);
  });

  it("honours the same link again after the key left the URL", async () => {
    const loc = mount("/runs/A?file=report.md", ["A"]);
    await waitFor(() => expect(fetchArtifactFile).toHaveBeenCalledTimes(1));
    act(() => loc.navigate("/runs/A"));
    act(() => loc.navigate("/runs/A?file=report.md"));
    await waitFor(() => expect(fetchArtifactFile).toHaveBeenCalledTimes(2));
  });

  it("writes the file into the URL on a click, keeping the run's other keys", async () => {
    const loc = mount("/runs/A?store=%2Fsrv%2Fstore", ["A"]);
    fireEvent.click(await screen.findByRole("link", { name: "report.md" }));
    await waitFor(() => expect(fetchArtifactFile).toHaveBeenCalledTimes(1));
    expect(loc.history[loc.history.length - 1]).toBe("/runs/A?store=%2Fsrv%2Fstore&file=report.md");
  });
});
