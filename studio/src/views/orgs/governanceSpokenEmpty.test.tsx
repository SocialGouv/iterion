// @vitest-environment jsdom
// The org admin's decision surface must render the SPOKEN-EMPTY gestures
// the gate parks BY NAME: a hold_labels [] lift, a label_allowlist []
// widen, an auto_fix false (zero-touch OFF), and the difference between a
// launch_vars_replace with a spoken map (a clear when empty) and one with
// NO map at all (the server keeps the stored pins). The three slices/maps
// collapse to absent keys on the wire (json omitempty), so the server
// marks each spoken field with a <field>_set fact and the card reads it —
// a silent record renders none of these lines.
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

function record(extra: Record<string, unknown>): Array<Record<string, unknown>> {
  return [
    {
      id: "pa-r5",
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

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <GovernanceTab orgID="o1" canManage />
    </QueryClientProvider>,
  );
}

async function renderCard(extra: Record<string, unknown>): Promise<string> {
  state.approvals = record(extra);
  renderTab();
  expect(await screen.findByText("org/api")).toBeTruthy();
  return document.body.textContent ?? "";
}

describe("GovernanceTab spoken-empty gestures on the approval card", () => {
  it("names the hold-label lift the approval executes (hold_labels_set, empty on the wire)", async () => {
    const body = await renderCard({ hold_labels_set: true });
    expect(body).toMatch(/hold label/i);
    expect(body).toMatch(/lift/i);
    // The confirm the admin clicks through says the same thing.
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await screen.findByText("Approve provisioning?");
    expect(document.body.textContent ?? "").toMatch(/lift/i);
  });

  it("names the dispatch widening (label_allowlist_set, empty on the wire)", async () => {
    const body = await renderCard({ label_allowlist_set: true });
    expect(body).toMatch(/label allowlist|widens/i);
    expect(body).toMatch(/widen|any label/i);
  });

  it("says zero-touch auto-fix turns OFF when the record carries auto_fix false", async () => {
    const body = await renderCard({ auto_fix: false });
    expect(body).toMatch(/auto-fix OFF/i);
  });

  it("keeps the ON wording for auto_fix true", async () => {
    const body = await renderCard({ auto_fix: true });
    expect(body).toMatch(/auto-fix ON/i);
  });

  it("tells a replace with NO map spoken from a clear — the stored pins stay", async () => {
    const body = await renderCard({ launch_vars_replace: true });
    expect(body).toMatch(/no map spoken/i);
    expect(body).toMatch(/stored pins stay/i);
    expect(body).not.toMatch(/clear/i);
  });

  it("keeps the whole-map clear wording for a spoken empty map (set + empty)", async () => {
    const body = await renderCard({ launch_vars_set: true, launch_vars_replace: true });
    expect(body).toMatch(/launch vars/i);
    expect(body).toMatch(/clear/i);
  });

  it("renders none of the spoken-empty lines on a silent record", async () => {
    const body = await renderCard({});
    expect(body).not.toMatch(/lift/i);
    expect(body).not.toMatch(/widen|any label/i);
    expect(body).not.toMatch(/auto-fix OFF/i);
    expect(body).not.toMatch(/no map spoken/i);
    expect(body).not.toMatch(/clear/i);
  });
});
