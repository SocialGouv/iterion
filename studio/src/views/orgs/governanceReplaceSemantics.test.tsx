// @vitest-environment jsdom
// The org admin's decision surface must describe EVERY record the server
// can hold — including the Replace shape the PATCH route parks (a swap of
// the bot set): approving it re-targets the integration to EXACTLY the
// requested bots, and the bots the current set holds that the request
// drops lose their webhook + schedules. A card that reads "Enable
// fixer-pr on org/api" while review-pr and docs-refresh are disabled
// hides the one fact the admin is deciding on. Same contract for
// launch_vars_replace: the spoken map REPLACES the stored vars wholesale
// (an empty map is a clear of every pin).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
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

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <GovernanceTab orgID="o1" canManage />
    </QueryClientProvider>,
  );
}

describe("GovernanceTab Replace-approval visibility", () => {
  it("names the bots a Replace approval disables — on the card and in the confirm", async () => {
    state.approvals = [
      // Exactly the record the PATCH route parks for a swap-shaped bot set
      // (Replace true, the live set snapshotted into base_bot_ids).
      {
        id: "pa-7",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["fixer-pr"],
        integration_id: "int-1",
        replace: true,
        base_bot_ids: ["review-pr", "docs-refresh"],
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    // The row itself renders — the witness is not vacuous.
    expect(await screen.findByText("org/api")).toBeTruthy();
    const body = document.body.textContent ?? "";
    expect(body).toContain("fixer-pr");
    // THE PROPERTY: the take-away is on the surface — which bots the
    // approval disables, and that the requested set REPLACES the live one.
    expect(body).toContain("review-pr");
    expect(body).toContain("docs-refresh");
    expect(body).toMatch(/replac/i);
    // The confirm the admin clicks through says the same thing.
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await screen.findByText("Approve provisioning?");
    const dialogBody = document.body.textContent ?? "";
    expect(dialogBody).toContain("review-pr");
    expect(dialogBody).toContain("docs-refresh");
    expect(dialogBody).toMatch(/replac/i);
  });

  it("says the launch vars replace the whole map — and clears read as clears", async () => {
    state.approvals = [
      {
        id: "pa-8",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["fixer-pr"],
        integration_id: "int-1",
        replace: true,
        base_bot_ids: ["fixer-pr"],
        launch_vars: {},
        launch_vars_replace: true,
        launch_vars_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    const body = document.body.textContent ?? "";
    // An empty spoken map beside launch_vars_replace is a CLEAR of every
    // stored pin — the admin must not read it as "no launch vars spoken".
    expect(body).toMatch(/launch vars/i);
    expect(body).toMatch(/replac/i);
    expect(body).toMatch(/clear/i);
  });

  it("marks a spoken non-empty map as a whole-map replacement, not a merge", async () => {
    state.approvals = [
      {
        id: "pa-9",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["review-pr"],
        integration_id: "int-1",
        replace: false,
        launch_vars: { gate_context: "api" },
        launch_vars_replace: true,
        launch_vars_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    const body = document.body.textContent ?? "";
    expect(body).toContain("gate_context=api");
    expect(body).toMatch(/launch vars replace/i);
  });
});
