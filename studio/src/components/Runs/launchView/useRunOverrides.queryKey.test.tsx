// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

// The preview call is the only thing the key can drive: mock it and count.
const apiMocks = vi.hoisted(() => ({
  previewRunCost: vi.fn(),
}));

vi.mock("@/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/runs")>();
  return { ...actual, previewRunCost: apiMocks.previewRunCost };
});

// The backend-detect store reads a fetch on mount; a stable empty report
// keeps this test about the query key alone.
vi.mock("@/store/backendDetect", () => ({
  useBackendDetectStore: (sel: (s: { report: null }) => unknown) => sel({ report: null }),
}));

import { useRunOverrides } from "./useRunOverrides";

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

afterEach(() => {
  cleanup();
  apiMocks.previewRunCost.mockReset();
});

describe("the preview query key", () => {
  it("re-queries when the codemode MODE changes — the screen answers differently", async () => {
    apiMocks.previewRunCost.mockResolvedValue({});
    const { result } = renderHook(
      () => useRunOverrides("", "agent a:\n  model: \"m\"\n", false),
      { wrapper },
    );
    await waitFor(() => expect(apiMocks.previewRunCost).toHaveBeenCalled());
    const before = apiMocks.previewRunCost.mock.calls.length;

    act(() => result.current.setCodemodeOverride("off"));
    await waitFor(() =>
      expect(apiMocks.previewRunCost.mock.calls.length).toBeGreaterThan(before),
    );
  });

  it("does NOT re-query when a BOUNDS field is typed — the request carries no bounds", async () => {
    // The revi gate on #3354: keying the bounds fired one POST (and a
    // fail-closed flicker) per keystroke, for a verdict no answer could
    // depend on.
    apiMocks.previewRunCost.mockResolvedValue({});
    const { result } = renderHook(
      () => useRunOverrides("", "agent a:\n  model: \"m\"\n", false),
      { wrapper },
    );
    await waitFor(() => expect(apiMocks.previewRunCost).toHaveBeenCalled());
    const before = apiMocks.previewRunCost.mock.calls.length;

    act(() => result.current.setCodemodeOverride("on"));
    await waitFor(() =>
      expect(apiMocks.previewRunCost.mock.calls.length).toBeGreaterThan(before),
    );
    const settled = apiMocks.previewRunCost.mock.calls.length;

    act(() =>
      result.current.setCodemodeFields((f) => ({ ...f, toolCalls: "4096" })),
    );
    // Give any stray refetch a chance to fire before the assertion.
    await new Promise((r) => setTimeout(r, 50));
    expect(apiMocks.previewRunCost.mock.calls.length).toBe(settled);
  });
});
