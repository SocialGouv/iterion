// @vitest-environment jsdom
// The legacy wire shape (a record parked before the Set flags shipped)
// with launch_vars_replace AND a non-empty map: the server's approve
// replay treats the non-nil map as a WRITE — the map applies. The card
// must render the map, not "no map spoken — the stored pins stay" (which
// denies a map the approval will execute). The "no map spoken" line keeps
// its meaning for the shape it was written for: replace spoken, no map on
// the wire at all.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const state = vi.hoisted(() => ({
  approvals: [] as Array<Record<string, unknown>>,
  approve: vi.fn(async () => undefined),
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
    approveProvision: state.approve,
  };
});

import GovernanceTab from "./GovernanceTab";

afterEach(cleanup);

beforeEach(() => {
  state.approvals = [];
  state.approve.mockClear();
});

function record(extra: Record<string, unknown>): Array<Record<string, unknown>> {
  return [
    {
      id: "pa-r6l",
      org_id: "o1",
      tenant_id: "t1",
      connection_id: "c1",
      repo_full_name: "org/api",
      bot_ids: ["review-pr"],
      requested_by: "u1",
      created_at: new Date().toISOString(),
      ...extra,
    },
  ];
}

async function renderCard(extra: Record<string, unknown>): Promise<string> {
  state.approvals = record(extra);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <GovernanceTab orgID="o1" canManage />
    </QueryClientProvider>,
  );
  expect(await screen.findByText("org/api")).toBeTruthy();
  return document.body.textContent ?? "";
}

describe("GovernanceTab legacy replace+map record", () => {
  it("renders the map the approve replay will apply, not a no-map-spoken claim", async () => {
    const body = await renderCard({
      launch_vars_replace: true,
      launch_vars: { gate_context: "api" },
    });
    expect(body).toMatch(/gate_context=api/);
    expect(body).not.toMatch(/no map spoken/);
  });

  it("keeps the no-map-spoken claim for replace with no map on the wire", async () => {
    const body = await renderCard({ launch_vars_replace: true });
    expect(body).toMatch(/no map spoken/);
    expect(body).not.toMatch(/gate_context/);
  });
});
