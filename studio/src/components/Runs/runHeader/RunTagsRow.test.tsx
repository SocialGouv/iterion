// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import RunTagsRow from "./RunTagsRow";

afterEach(cleanup);

describe("RunTagsRow", () => {
  it("associates the visible Tags heading with the tag field (RGAA 11.1)", () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    client.setQueryData(["run-tags", "run-1"], ["release"]);
    vi.spyOn(client, "fetchQuery").mockResolvedValue(["release"]);
    render(
      <QueryClientProvider client={client}>
        <RunTagsRow runId="run-1" />
      </QueryClientProvider>,
    );
    const field = screen.getByLabelText("Tags") as HTMLInputElement;
    expect(field.id).toBe("run-tags-input");
  });
});
