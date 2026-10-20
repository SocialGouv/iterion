// @vitest-environment jsdom
// The org admin's decision surface: a parked request's routing block — the
// ONE payload the ex-ante gate exists to arbitrate — must be VISIBLE on the
// approval card. These witnesses render the REAL tab against the REAL wire
// shape the server serializes (forge.ProvisionApproval's json tags: routing,
// routing_set) and assert the decision payload reaches the DOM — the pin
// itself, and the CLEARED case (a request that wipes a previously approved
// block must not look like a plain bot-only request).
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

describe("GovernanceTab approval-card routing visibility", () => {
  it("shows the parked routing block to the approving admin", async () => {
    state.approvals = [
      // Exactly what the server's approvals list returns for a parked
      // routing write (the Go wire probe pins the tags).
      {
        id: "pa-1",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["review-pr"],
        routing: { overrides: [{ selector: "*", backend: "byo" }] },
        routing_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    // The row itself renders — the witness is not vacuous.
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(screen.getByText("review-pr")).toBeTruthy();
    // THE PROPERTY: the decision payload (the routing block, here a `*` pin
    // onto the "byo" backend) is on the card the admin approves.
    expect(document.body.textContent ?? "").toMatch(/routing: \* → byo/);
  });

  it("shows a spoken routing CLEAR — it must not read as a bot-only request", async () => {
    state.approvals = [
      {
        id: "pa-2",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["review-pr"],
        routing: null,
        routing_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").toContain("routing: CLEARED");
  });

  it("renders a policy without overrides verbatim — never a silent omission", async () => {
    state.approvals = [
      {
        id: "pa-3",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["review-pr"],
        routing: { providers: ["byo"] },
        routing_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").toContain('routing: {"providers":["byo"]}');
  });

  // An override is ONE cell of the policy: the effort slot it carries and
  // the policy-level cells beside it (the providers allowlist is the
  // unattended-spend gate, strict turns pins into refusals) are exactly
  // what the admin is approving — none may vanish behind the pin line.
  it("shows an override's effort cell, not just its target", async () => {
    state.approvals = [
      {
        id: "pa-4",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["review-pr"],
        routing: { overrides: [{ selector: "*", backend: "claw", effort: "high" }] },
        routing_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").toMatch(/routing: \* → claw/);
    expect(document.body.textContent ?? "").toContain("effort: high");
  });

  it("shows the policy-level cells beside present overrides", async () => {
    state.approvals = [
      {
        id: "pa-5",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["review-pr"],
        routing: {
          overrides: [{ selector: "*", backend: "byo" }],
          providers: ["forfait-claude"],
        },
        routing_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").toMatch(/routing: \* → byo/);
    expect(document.body.textContent ?? "").toContain("forfait-claude");
  });

  it("shows strict beside present overrides", async () => {
    state.approvals = [
      {
        id: "pa-6",
        org_id: "o1",
        tenant_id: "t1",
        connection_id: "c1",
        repo_full_name: "org/api",
        bot_ids: ["review-pr"],
        routing: {
          overrides: [{ selector: "review*", model: "gemini-3-pro" }],
          strict: true,
        },
        routing_set: true,
        requested_by: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderTab();
    expect(await screen.findByText("org/api")).toBeTruthy();
    expect(document.body.textContent ?? "").toMatch(/routing: review\* → gemini-3-pro/);
    // The non-override cell renders verbatim.
    expect(document.body.textContent ?? "").toContain('"strict":true');
  });
});
