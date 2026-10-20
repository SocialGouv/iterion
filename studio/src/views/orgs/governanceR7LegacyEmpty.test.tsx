// @vitest-environment jsdom
// Round-7 witnesses on the approval card.
//
//   - A legacy record (parked before the Set flags shipped) that spoke
//     launch_vars {} beside launch_vars_replace replays into a WHOLE-MAP
//     clear, and the wire now carries the empty-but-present map — the card
//     must render the clear, not "no map spoken — the stored pins stay"
//     (which denies a gesture the approval executes). The silent legacy
//     twin (null map) keeps the no-map-spoken claim: the contrast is the
//     witness.
//   - The spoken-empty CLEAR branches carry the same base delta the
//     partial branches got: a whole-pause lift re-arms exactly the labels
//     the base recorded, and a whole-allowlist widen re-arms exactly the
//     stored allowlist — naming neither label reads as the whole story.
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
      id: "pa-r7",
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

describe("GovernanceTab legacy spoken-empty records", () => {
  it("renders the whole-map clear for a legacy {} + replace (empty-but-present, no flag)", async () => {
    const body = await renderCard({ launch_vars_replace: true, launch_vars: {} });
    expect(body).toMatch(/launch vars/i);
    expect(body).toMatch(/clears every existing pin/i);
    expect(body).not.toMatch(/no map spoken/);
  });

  it("keeps the no-map-spoken claim for the legacy silent twin (null map)", async () => {
    const body = await renderCard({ launch_vars_replace: true, launch_vars: null });
    expect(body).toMatch(/no map spoken/);
    expect(body).not.toMatch(/clears every existing pin/);
  });

  it("names the lifted labels on a hold_labels [] clear over a recorded base", async () => {
    const body = await renderCard({
      hold_labels_set: true,
      hold_labels: [],
      base_hold_labels: ["pause-a", "pause-b"],
    });
    expect(body).toMatch(/lifts the automation pause/i);
    expect(body).toMatch(/pause-a, pause-b/);
  });

  it("keeps the bare lift wording when no base is recorded", async () => {
    const body = await renderCard({ hold_labels_set: true });
    expect(body).toMatch(/lifts the automation pause/i);
    expect(body).not.toMatch(/— lifts:/);
  });

  it("names the widened allowlist on a label_allowlist [] clear over a recorded base", async () => {
    const body = await renderCard({
      label_allowlist_set: true,
      label_allowlist: [],
      base_label_allowlist: ["implement"],
    });
    expect(body).toMatch(/widens dispatch to any label/i);
    expect(body).toMatch(/widens to: implement/);
  });

  it("renders the same clear wording for a legacy flagless [] hold lift", async () => {
    const body = await renderCard({ hold_labels: [], base_hold_labels: ["pause-a"] });
    expect(body).toMatch(/lifts the automation pause/i);
    expect(body).toMatch(/pause-a/);
  });
});
