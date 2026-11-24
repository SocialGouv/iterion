// @vitest-environment jsdom
// The org admin's decision surface for a parked POOL block (ADR-164): which
// key pool would fund the repo's unattended runs is the payload the ex-ante
// gate arbitrates — the server parks every spoken block, so the approval
// card must show it, its opt-out, and its CLEAR (a request whose only change
// is the pool block must not read as a plain bot-only request). The REAL tab
// renders the REAL wire shape (forge.ProvisionApproval's json tags: pool,
// pool_set).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const state = vi.hoisted(() => ({
  approvals: [] as Array<Record<string, unknown>>,
}));

vi.mock("@/api/orgGovernance", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/orgGovernance")>("@/api/orgGovernance");
  return {
    ...actual,
    getOrgSettings: vi.fn(async () => ({ require_provision_approval: true })),
    getOrgRoutingPolicy: vi.fn(async () => null),
    listOrgTeamSummaries: vi.fn(async () => []),
    listOrgProvisionApprovals: vi.fn(async () => state.approvals),
  };
});

import GovernanceTab from "./GovernanceTab";

afterEach(cleanup);

beforeEach(() => {
  state.approvals = [];
});

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <GovernanceTab orgID="o1" canManage />
    </QueryClientProvider>,
  );
}

function parked(extra: Record<string, unknown>): Record<string, unknown> {
  return {
    id: "pa-pool",
    org_id: "o1",
    tenant_id: "t1",
    connection_id: "c1",
    repo_full_name: "org/api",
    bot_ids: ["review-pr"],
    requested_by: "u1",
    created_at: new Date().toISOString(),
    ...extra,
  };
}

describe("GovernanceTab approval-card pool visibility", () => {
  it("shows the parked pool block and its strategy", async () => {
    state.approvals = [
      parked({
        pool: { pool_id: "pool-forfaits", enabled: true, strategy: { mode: "priority", margin_pct: 80 } },
        pool_set: true,
      }),
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    const text = document.body.textContent ?? "";
    expect(text).toContain("pool: pool-forfaits");
    expect(text).toContain('"margin_pct":80');
  });

  it("shows an opt-out block as OPTED OUT", async () => {
    state.approvals = [parked({ pool: { pool_id: "pool-forfaits", enabled: false }, pool_set: true })];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").toContain("pool: pool-forfaits (OPTED OUT)");
  });

  it("shows a spoken pool CLEAR — it must not read as a bot-only request", async () => {
    state.approvals = [parked({ pool: null, pool_set: true })];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").toContain("pool: CLEARED");
  });

  it("says nothing about the pool when the request did not speak about it", async () => {
    state.approvals = [parked({})];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").not.toContain("pool:");
  });
});
