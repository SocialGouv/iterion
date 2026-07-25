// @vitest-environment jsdom

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  fetchArtifactFile: vi.fn(),
}));

vi.mock("@/api/runs", () => ({
  fetchArtifactFile: apiMocks.fetchArtifactFile,
}));

import { usePreview } from "./usePreview";

afterEach(() => {
  cleanup();
  apiMocks.fetchArtifactFile.mockReset();
});

describe("usePreview", () => {
  it("does not restore stale text after the preview is closed during decoding", async () => {
    let finishText: ((value: string) => void) | undefined;
    const text = vi.fn(
      () =>
        new Promise<string>((resolve) => {
          finishText = resolve;
        }),
    );
    apiMocks.fetchArtifactFile.mockResolvedValue({
      blob: { text } as unknown as Blob,
      contentType: "text/plain",
    });

    const { result } = renderHook(() => usePreview("run-1"));
    act(() => {
      result.current.openPreview({ path: "notes.txt", size: 12 });
    });
    await waitFor(() => expect(text).toHaveBeenCalledTimes(1));

    act(() => {
      result.current.closePreview();
    });
    expect(result.current.preview).toBeNull();
    if (!finishText) throw new Error("text decoding did not start");
    const resolveText = finishText;

    await act(async () => {
      resolveText("stale contents");
      await Promise.resolve();
    });
    expect(result.current.preview).toBeNull();
  });
});
