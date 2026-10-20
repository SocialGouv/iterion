// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import FilePicker from "./FilePicker";

afterEach(cleanup);

describe("FilePicker", () => {
  it("names its search field — a placeholder alone is not a label (RGAA 11.1)", () => {
    render(
      <QueryClientProvider client={new QueryClient()}>
        <FilePicker open onOpenChange={() => {}} onPick={vi.fn()} />
      </QueryClientProvider>,
    );
    expect(screen.getByRole("textbox", { name: "Search files" })).toBeTruthy();
  });
});
