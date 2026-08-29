// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  fetchModelCapabilities: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  fetchModelCapabilities: apiMocks.fetchModelCapabilities,
}));

import { useModelCapabilities } from "./useModelCapabilities";

function wrapper(client: QueryClient) {
  return ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  apiMocks.fetchModelCapabilities.mockResolvedValue({
    provider: "anthropic",
    model: "claude-opus-5",
    spec: "anthropic/claude-opus-5",
    source: "curated",
    context_window: 200_000,
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  apiMocks.fetchModelCapabilities.mockReset();
});

// Every picker feeding this hook is a free-text <Input>, so a spec that is
// keyed straight into the query issues one authenticated round trip per
// CHARACTER — and leaves each half-typed prefix in the cache. The settling
// window is what makes the caption cost one request per model, not per
// keystroke.
describe("useModelCapabilities", () => {
  it("issues one lookup for a typed-out spec, not one per keystroke", async () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const { rerender } = renderHook(
      ({ spec }: { spec: string }) => useModelCapabilities(spec),
      { wrapper: wrapper(client), initialProps: { spec: "" } },
    );

    for (const prefix of ["a", "an", "ant", "anthropic/claude-opus-5"]) {
      rerender({ spec: prefix });
    }
    // Nothing has settled yet, so nothing has been asked for.
    expect(apiMocks.fetchModelCapabilities).not.toHaveBeenCalled();

    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    await waitFor(() =>
      expect(apiMocks.fetchModelCapabilities).toHaveBeenCalledTimes(1),
    );
    expect(apiMocks.fetchModelCapabilities).toHaveBeenCalledWith(
      "anthropic/claude-opus-5",
      expect.anything(),
    );
  });

  // gcTime governs how long an UNMOUNTED key is retained. Infinite would mean
  // the prefixes debouncing did not absorb accumulate for the life of the page
  // and are never collected; `staleTime: Infinity` is what actually pins a
  // settled aggregator answer, and it is unaffected.
  it("resolves a mounted spec and bounds what it retains", async () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const { result } = renderHook(
      () => useModelCapabilities("anthropic/claude-opus-5"),
      { wrapper: wrapper(client) },
    );

    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    await waitFor(() => expect(result.current.capabilities).not.toBeNull());

    const entry = client
      .getQueryCache()
      .find({ queryKey: ["model-capabilities", "anthropic/claude-opus-5"] });
    expect(entry).toBeDefined();
    expect(Number.isFinite(entry?.gcTime ?? Number.POSITIVE_INFINITY)).toBe(
      true,
    );
  });
  // The cold-start case the whole caching rule exists for, end to end: the
  // server answers `curated` with no price while its background models.dev
  // fetch is still in flight, then answers `aggregator` seconds later. A
  // MOUNTED picker must pick that up — staleTime alone does not refetch, and
  // the studio disables focus-refetching, so without the poll this answer
  // stood until the picker was remounted and the operator watching it never
  // saw a price.
  it("supersedes a cold curated answer once the server refresh lands", async () => {
    apiMocks.fetchModelCapabilities.mockReset();
    apiMocks.fetchModelCapabilities
      .mockResolvedValueOnce({
        provider: "anthropic",
        model: "claude-opus-5",
        spec: "anthropic/claude-opus-5",
        source: "curated",
        context_window: 200_000,
        max_output_tokens: 0,
        input_cost_per_m: 0,
        output_cost_per_m: 0,
      })
      .mockResolvedValue({
        provider: "anthropic",
        model: "claude-opus-5",
        spec: "anthropic/claude-opus-5",
        source: "aggregator",
        context_window: 1_000_000,
        max_output_tokens: 64_000,
        input_cost_per_m: 5,
        output_cost_per_m: 25,
      });

    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false, refetchOnWindowFocus: false },
      },
    });
    const { result } = renderHook(
      () => useModelCapabilities("anthropic/claude-opus-5"),
      { wrapper: wrapper(client) },
    );

    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    await waitFor(() =>
      expect(result.current.capabilities?.source).toBe("curated"),
    );

    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    await waitFor(
      () => expect(result.current.capabilities?.source).toBe("aggregator"),
      { timeout: 2000 },
    );
    expect(result.current.capabilities?.input_cost_per_m).toBe(5);

    // …and it stops there: a settled answer must not keep polling.
    const settled = apiMocks.fetchModelCapabilities.mock.calls.length;
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    expect(apiMocks.fetchModelCapabilities.mock.calls.length).toBe(settled);
  });
});
