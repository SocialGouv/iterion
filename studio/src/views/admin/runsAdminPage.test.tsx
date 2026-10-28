// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { AdminRunsQuery, AdminRunsResponse } from "@/api/adminRuns";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

// The runs console screen (#2148): rows name their org, sovereign-pool
// rows stay marked, and the org + state filters travel to the server
// COMBINED. Mounted without the RequireSuperAdmin wrapper — the only
// altitude at which the per-page gate is observable (see
// adminScreens.test.tsx for the shared rationale).

let mockUser: { is_super_admin?: boolean } | null = null;
let mockInfo: { mode?: string } | null = null;

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: mockUser }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) => sel({ info: mockInfo }),
}));

const listAdminRuns = vi.fn<(q?: AdminRunsQuery) => Promise<AdminRunsResponse>>();
const listAdminOrgs = vi.fn<() => Promise<{ id: string; name: string }[]>>();

vi.mock("@/api/adminRuns", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/adminRuns")>();
  return {
    ...actual,
    listAdminRuns: (q?: AdminRunsQuery) => listAdminRuns(q),
    listAdminOrgs: () => listAdminOrgs(),
  };
});

import RunsAdminPage from "./RunsAdminPage";
import { ADMIN_BASE } from "./adminRoutes";

setupMatchMedia();

const PAGE: AdminRunsResponse = {
  runs: [
    {
      id: "run-1",
      workflow_name: "wf-alpha",
      status: "running",
      created_at: "2026-10-27T10:00:00Z",
      updated_at: "2026-10-27T10:05:00Z",
      team_id: "team-a1",
      team_name: "Team A1",
      org_id: "org-alpha",
      org_name: "Alpha Org",
      source_kind: "manual",
    },
    {
      id: "run-2",
      workflow_name: "wf-beta",
      status: "failed",
      created_at: "2026-10-27T09:00:00Z",
      updated_at: "2026-10-27T09:10:00Z",
      team_id: "team-a2",
      team_name: "Team A2",
      org_id: "org-alpha",
      org_name: "Alpha Org",
      runner_pool: "pool-souverain",
      source_kind: "dispatcher",
    },
  ],
};

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const loc = memoryLocation({ path: `/studio${ADMIN_BASE}/runs` });
  return render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base="/studio">
        <Route path={ADMIN_BASE} nest>
          <Route path="/runs" component={RunsAdminPage} />
        </Route>
      </Router>
    </QueryClientProvider>,
  );
}

describe("RunsAdminPage", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("names each row's organization and marks the sovereign pool", async () => {
    mockUser = { is_super_admin: true };
    mockInfo = { mode: "cloud" };
    listAdminRuns.mockResolvedValue(PAGE);
    listAdminOrgs.mockResolvedValue([{ id: "org-alpha", name: "Alpha Org" }]);
    const view = mount();

    const table = await screen.findByRole("table", {
      name: /runs across every organization/i,
    });
    expect(table.textContent).toContain("Alpha Org");
    expect(table.textContent).toContain("Team A1");
    expect(table.textContent).toContain("pool: pool-souverain");
    const link = await screen.findByRole("link", { name: "run-1" });
    expect(link.getAttribute("href")).toContain("/runs/run-1");
    await expectNoViolationsStrict(view.container, "RunsAdminPage");
  });

  it("sends the org and state filters to the server COMBINED", async () => {
    mockUser = { is_super_admin: true };
    mockInfo = { mode: "cloud" };
    listAdminRuns.mockResolvedValue({ runs: [] });
    listAdminOrgs.mockResolvedValue([{ id: "org-alpha", name: "Alpha Org" }]);
    mount();

    await screen.findByLabelText("Organization");
    // The org options load async — wait for the option to exist, or the
    // change lands on an empty dropdown and React resets it to "".
    await screen.findByRole("option", { name: "Alpha Org" });
    fireEvent.change(screen.getByLabelText("Organization"), {
      target: { value: "org-alpha" },
    });
    fireEvent.change(screen.getByLabelText("State"), {
      target: { value: "failed" },
    });
    await waitFor(() => {
      expect(listAdminRuns).toHaveBeenCalledWith(
        expect.objectContaining({ orgId: "org-alpha", status: "failed" }),
      );
    });
  });

  it("stays behind the super-admin gate without the wrapper", async () => {
    mockUser = { is_super_admin: false };
    mockInfo = { mode: "cloud" };
    mount();
    expect(await screen.findByText("Super-admin only.")).toBeTruthy();
    expect(listAdminRuns).not.toHaveBeenCalled();
  });
});
