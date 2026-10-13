// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { OrgView, TeamSummary } from "@/api/orgs";
import type { OrgUsage } from "@/api/usage";
import { setupMatchMedia, expectNoViolations } from "@/__tests__/a11y/axeHelpers";

// The org drawer is the console's teams drill-down (its sections are
// read-only or form fields; the mutations belong to the sections' own
// surfaces). Its two reads are stubbed so this exercises the drawer's
// landmark/heading/label wiring and the drill-down table, not the API.
const getAdminOrgUsage = vi.fn<() => Promise<OrgUsage>>();
const listAdminOrgTeams = vi.fn<() => Promise<TeamSummary[]>>();

vi.mock("@/api/usage", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/usage")>();
  return { ...actual, getAdminOrgUsage: () => getAdminOrgUsage() };
});
vi.mock("@/api/orgs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgs")>();
  return { ...actual, listAdminOrgTeams: () => listAdminOrgTeams() };
});

import { OrgDrawer } from "./OrgDrawer";

setupMatchMedia();

const ORG: OrgView = {
  id: "org-1",
  name: "Org One",
  slug: "org-one",
  status: "active",
  monthly_run_quota: 100,
  memory_quota_bytes: 1024 * 1024 * 1024,
};

const USAGE: OrgUsage = {
  org: { id: ORG.id, name: ORG.name, slug: ORG.slug, status: ORG.status },
  members: 3,
  teams: 2,
  effective_memory_quota_bytes: 1024 * 1024 * 1024,
  monthly_run_quota: 100,
  runs_this_month: 7,
  cost_usd_this_month: 1.5,
  input_tokens_this_month: 10,
  output_tokens_this_month: 20,
  active_runs: 0,
  webhook_calls_this_month: 0,
  memory_used_bytes: 0,
  api_key_count: 1,
  generic_secret_count: 0,
  bot_binding_count: 0,
  webhook_count: 0,
};

const TEAMS: TeamSummary[] = [
  { id: "team-1", name: "Team One", slug: "team-one", status: "active", max_concurrent_runs: 4 },
  { id: "team-2", name: "Team Two", slug: "team-two", status: "suspended" },
];

function mountDrawer() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={qc}>
      <OrgDrawer
        org={ORG}
        busy={false}
        isActiveOrg={false}
        onClose={() => {}}
        onChanged={() => {}}
        onAfterUpdate={() => Promise.resolve()}
        reloadIdentity={() => Promise.resolve()}
        run={() => Promise.resolve()}
      />
    </QueryClientProvider>,
  );
  // Radix renders the dialog in a portal, outside render()'s container.
  return screen.getByRole("dialog");
}

beforeEach(() => {
  getAdminOrgUsage.mockReset().mockResolvedValue(USAGE);
  listAdminOrgTeams.mockReset().mockResolvedValue(TEAMS);
});
afterEach(cleanup);

describe("OrgDrawer — teams drill-down", () => {
  it("lists the org's teams with their status and caps", async () => {
    mountDrawer();
    expect(await screen.findByRole("table", { name: "Teams in this organization" })).toBeTruthy();
    for (const team of TEAMS) {
      expect(screen.getByText(team.name)).toBeTruthy();
      expect(screen.getByText(team.slug, { exact: false })).toBeTruthy();
    }
    expect(screen.getByText("(2)")).toBeTruthy();
    expect(listAdminOrgTeams).toHaveBeenCalled();
  });

  it("says so, rather than showing an empty table, when the org has no team", async () => {
    listAdminOrgTeams.mockResolvedValue([]);
    mountDrawer();
    expect(await screen.findByText("This organization has no teams.")).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Teams in this organization" })).toBeNull();
  });
});

describe("a11y / OrgDrawer", () => {
  it("has no violation once its usage and teams have loaded", async () => {
    const dialog = mountDrawer();
    await screen.findByRole("table", { name: "Teams in this organization" });
    await expectNoViolations(dialog, "OrgDrawer");
  });

  it("has no violation while the drill-down is still loading", async () => {
    let release: (teams: TeamSummary[]) => void = () => {};
    listAdminOrgTeams.mockReturnValue(
      new Promise<TeamSummary[]>((resolve) => {
        release = resolve;
      }),
    );
    const dialog = mountDrawer();
    await screen.findByText("Teams");
    // The pending state is a skeleton: neither an empty table nor the
    // "no teams" verdict, which would be a lie about an org that has some.
    expect(screen.queryByRole("table", { name: "Teams in this organization" })).toBeNull();
    expect(screen.queryByText("This organization has no teams.")).toBeNull();
    await expectNoViolations(dialog, "OrgDrawer (loading)");
    release(TEAMS);
  });
});
