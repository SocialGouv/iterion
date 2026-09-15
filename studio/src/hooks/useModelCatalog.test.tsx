// @vitest-environment jsdom

import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useModelCatalog } from "./useModelCatalog";

const api = vi.hoisted(() => ({ fetchModels: vi.fn() }));
vi.mock("@/api/models", () => api);

const emptyCatalog = { models: [] };

describe("useModelCatalog refresh", () => {
  beforeEach(() => vi.resetAllMocks());

  it("keeps refresh enabled across a react-query retry", async () => {
    api.fetchModels
      .mockResolvedValueOnce(emptyCatalog)
      .mockRejectedValueOnce(new Error("models.dev unavailable"))
      .mockResolvedValueOnce(emptyCatalog);
    const client = new QueryClient({
      defaultOptions: { queries: { retry: 1, retryDelay: 0 } },
    });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
    const { result } = renderHook(() => useModelCatalog(), { wrapper });
    await waitFor(() => expect(api.fetchModels).toHaveBeenCalledTimes(1));

    act(() => result.current.refresh());

    await waitFor(() => expect(api.fetchModels).toHaveBeenCalledTimes(3));
    expect(api.fetchModels.mock.calls[1]?.[0].refresh).toBe(true);
    expect(api.fetchModels.mock.calls[2]?.[0].refresh).toBe(true);
  });
});
