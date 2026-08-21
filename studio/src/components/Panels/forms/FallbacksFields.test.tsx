// @vitest-environment jsdom
import type { ReactElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FallbackDecl } from "@/api/types";
import FallbacksFields from "./FallbacksFields";

afterEach(cleanup);

function wrap(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

describe("FallbacksFields", () => {
  it("renders authored routes with a shortened model label", () => {
    wrap(
      <FallbacksFields
        value={[{ name: "terra", model: "openai-codex/gpt-5.6-terra" }]}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText(/terra → gpt-5\.6-terra/)).toBeTruthy();
    expect(
      (screen.getByDisplayValue("openai-codex/gpt-5.6-terra") as HTMLInputElement).value,
    ).toBe("openai-codex/gpt-5.6-terra");
  });

  it("adds a named route through onChange", () => {
    const onChange = vi.fn();
    wrap(<FallbacksFields value={undefined} onChange={onChange} />);
    fireEvent.click(screen.getByText("Fallbacks"));
    fireEvent.click(screen.getByRole("button", { name: "Add route" }));
    expect(onChange).toHaveBeenCalledTimes(1);
    const next = onChange.mock.calls[0]![0] as FallbackDecl[];
    expect(next).toHaveLength(1);
    expect(next[0]!.name).toBe("api");
  });
});
