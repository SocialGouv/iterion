// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ModelCapabilities } from "@/api/client";
import { ModelCapsCaption } from "./ModelCapsCaption";

const fetchModelCapabilities = vi.fn<(spec: string) => Promise<ModelCapabilities>>();
const fetchResolvedModel = vi.fn<(literal: string) => Promise<string>>();

vi.mock("@/api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/client")>();
  return {
    ...actual,
    fetchModelCapabilities: (spec: string) => fetchModelCapabilities(spec),
    fetchResolvedModel: (literal: string) => fetchResolvedModel(literal),
  };
});

function caps(over: Partial<ModelCapabilities> = {}): ModelCapabilities {
  return {
    spec: "anthropic/claude-opus-5",
    model: "claude-opus-5",
    source: "aggregator",
    context_window: 1_000_000,
    max_output_tokens: 64_000,
    input_cost_per_m: 5,
    output_cost_per_m: 25,
    ...over,
  };
}

function renderWithQuery(node: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    qc,
    ...render(<QueryClientProvider client={qc}>{node}</QueryClientProvider>),
  };
}

beforeEach(() => {
  fetchModelCapabilities.mockReset();
  fetchResolvedModel.mockReset();
});
afterEach(cleanup);

describe("ModelCapsCaption — which model it asks about", () => {
  it("uses the override when the operator typed one", async () => {
    fetchModelCapabilities.mockResolvedValue(caps({ spec: "openai/gpt-5.5" }));
    renderWithQuery(
      <ModelCapsCaption override="openai/gpt-5.5" authored="anthropic/claude-opus-5" />,
    );

    await waitFor(() =>
      expect(fetchModelCapabilities).toHaveBeenCalledWith("openai/gpt-5.5"),
    );
  });

  // The launch form's inherit path: the input value is empty and the node's
  // model lives only in the placeholder. This is most launches, so a caption
  // that needed the input value would be blank almost always.
  it("falls back to the authored model when nothing was typed", async () => {
    fetchModelCapabilities.mockResolvedValue(caps());
    renderWithQuery(<ModelCapsCaption authored="anthropic/claude-opus-5" />);

    await waitFor(() =>
      expect(fetchModelCapabilities).toHaveBeenCalledWith("anthropic/claude-opus-5"),
    );
    expect((await screen.findByTestId("model-caps-caption")).textContent).toBe(
      "1M context · 64K max out · $5.00 / $25.00 per M · aggregator",
    );
  });

  it("asks about the expansion of an env literal, never the template", async () => {
    fetchResolvedModel.mockResolvedValue("openai/gpt-5.5");
    fetchModelCapabilities.mockResolvedValue(caps({ spec: "openai/gpt-5.5" }));
    renderWithQuery(<ModelCapsCaption authored="${CODEX_MODEL:-openai/gpt-5.5}" />);

    await waitFor(() =>
      expect(fetchModelCapabilities).toHaveBeenCalledWith("openai/gpt-5.5"),
    );
    expect(fetchModelCapabilities).not.toHaveBeenCalledWith(
      "${CODEX_MODEL:-openai/gpt-5.5}",
    );
  });

  it("serves a bare model id — .bot files pin them", async () => {
    fetchModelCapabilities.mockResolvedValue(
      caps({ spec: "claude-opus-5", provider: undefined }),
    );
    renderWithQuery(<ModelCapsCaption override="claude-opus-5" />);

    await waitFor(() =>
      expect(fetchModelCapabilities).toHaveBeenCalledWith("claude-opus-5"),
    );
    expect((await screen.findByTestId("model-caps-caption")).textContent).toContain(
      "1M context",
    );
  });

  it("renders nothing at all when no model is selected", async () => {
    renderWithQuery(<ModelCapsCaption />);
    expect(screen.queryByTestId("model-caps-caption")).toBeNull();
    expect(fetchModelCapabilities).not.toHaveBeenCalled();
  });
});

describe("ModelCapsCaption — the cold start", () => {
  // Spec resolution is non-blocking: a cold lookup answers from the curated
  // table and only STARTS the background refresh, so the first response
  // carries no price. This asserts the RENDER half — the caption re-renders
  // in place when the better answer lands, with no remount. The caching rule
  // that gets the refetch to happen at all is asserted directly in
  // useModelCapabilities.test.ts; forcing a refetch here would pass just as
  // happily against "cache forever", which is the bug.
  it("re-renders in place when the aggregator answer replaces the curated one", async () => {
    fetchModelCapabilities
      .mockResolvedValueOnce(
        caps({
          source: "curated",
          max_output_tokens: 0,
          input_cost_per_m: 0,
          output_cost_per_m: 0,
        }),
      )
      .mockResolvedValue(caps());

    const { qc } = renderWithQuery(
      <ModelCapsCaption authored="anthropic/claude-opus-5" />,
    );

    const first = await screen.findByTestId("model-caps-caption");
    expect(first.textContent).toBe("1M context · price unknown · curated");

    // Stands in for the curated answer's stale window elapsing.
    await qc.invalidateQueries({ queryKey: ["model-capabilities"] });

    await waitFor(() =>
      expect(screen.getByTestId("model-caps-caption").textContent).toContain(
        "$5.00 / $25.00 per M · aggregator",
      ),
    );
    expect(fetchModelCapabilities).toHaveBeenCalledTimes(2);
  });

  // The mirror of the above: an aggregator answer cannot improve, so it must
  // not be refetched on every remount.
  it("keeps an aggregator answer without refetching", async () => {
    fetchModelCapabilities.mockResolvedValue(caps());
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    const { unmount } = render(
      <QueryClientProvider client={qc}>
        <ModelCapsCaption authored="anthropic/claude-opus-5" />
      </QueryClientProvider>,
    );
    await screen.findByTestId("model-caps-caption");
    unmount();

    render(
      <QueryClientProvider client={qc}>
        <ModelCapsCaption authored="anthropic/claude-opus-5" />
      </QueryClientProvider>,
    );
    await screen.findByTestId("model-caps-caption");

    expect(fetchModelCapabilities).toHaveBeenCalledTimes(1);
  });
});
