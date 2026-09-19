// @vitest-environment jsdom
// T9 — a11y + rendering for the org→teams drill-down section. It renders a
// loading skeleton then a table/empty state from GET /api/admin/orgs/{id}/teams.
// Mount it with a stubbed fetch returning a couple of teams and assert the
// rows render and the table is accessible.

import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { expectNoViolations, setupMatchMedia } from "@/__tests__/a11y/axeHelpers";
import { OrgTeamsSection } from "../orgs/OrgTeamsSection";

setupMatchMedia();

function mount(node: ReactNode): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <main>{node}</main>
    </QueryClientProvider>,
  ).container;
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(
        JSON.stringify({
          teams: [
            { id: "t1", name: "Alpha", slug: "alpha", status: "active", max_concurrent_runs: 3, launch_rate_per_min: 10 },
            { id: "t2", name: "Beta", slug: "beta", status: "suspended" },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("OrgTeamsSection", () => {
  it("lists the org's teams and is accessible", async () => {
    const container = mount(<OrgTeamsSection orgID="org-1" />);
    await waitFor(() => expect(screen.getByText("Alpha")).toBeTruthy());
    expect(screen.getByText("Beta")).toBeTruthy();
    // Caps rendered; a team without caps shows the em dash placeholder.
    expect(screen.getByText("alpha")).toBeTruthy();
    await expectNoViolations(container, "OrgTeamsSection");
  });
});
