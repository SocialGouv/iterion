// @vitest-environment jsdom
// A PARTIAL hold-label lift (stored [a b], spoken [a]) and an allowlist
// widening (stored [x], spoken [x y]) park precisely because they re-arm
// automation — and the destination set alone does not tell the approver
// WHAT the change lifts or widens. The record carries the base sets
// (base_hold_labels / base_label_allowlist, the base_bot_ids precedent)
// and the card renders the delta, mirroring the replace branch's
// "— disables: X". A record without a base (new repo, or an empty stored
// set — nothing can be lifted from it) renders no delta claim.
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

function baseRecord(): Record<string, unknown> {
  return {
    id: "pa-r6",
    org_id: "o1",
    tenant_id: "t1",
    connection_id: "c1",
    repo_full_name: "org/api",
    bot_ids: ["review-pr"],
    requested_by: "u1",
    created_at: new Date().toISOString(),
  };
}

async function renderCard(extra: Record<string, unknown>): Promise<string> {
  state.approvals = [{ ...baseRecord(), ...extra }];
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <GovernanceTab orgID="o1" canManage />
    </QueryClientProvider>,
  );
  expect(await screen.findByText("org/api")).toBeTruthy();
  return document.body.textContent ?? "";
}

describe("GovernanceTab renders the delta a partial lift or widen acts on", () => {
  it("names the label a partial hold-label lift removes from the pause", async () => {
    const body = await renderCard({
      hold_labels: ["pause-a"],
      hold_labels_set: true,
      base_hold_labels: ["pause-a", "pause-b"],
    });
    expect(body).toMatch(/hold labels: pause-a/);
    expect(body).toMatch(/lifts: pause-b/);
  });

  it("names the label an allowlist widening adds to dispatch", async () => {
    const body = await renderCard({
      label_allowlist: ["implement", "review"],
      base_label_allowlist: ["implement"],
    });
    expect(body).toMatch(/label allowlist: implement, review/);
    expect(body).toMatch(/widens to: review/);
  });

  it("claims no delta when no base is recorded (new repo, nothing stored)", async () => {
    const body = await renderCard({
      hold_labels: ["pause-x"],
      hold_labels_set: true,
    });
    expect(body).toMatch(/hold labels: pause-x/);
    expect(body).not.toMatch(/lifts:/);
  });
});
