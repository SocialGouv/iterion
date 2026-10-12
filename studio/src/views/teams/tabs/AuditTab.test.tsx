// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { OrgTreeView } from "@/api/auth";

// This tab mounts in two routers: the team page (router base /studio) and the
// platform audit console (/studio/admin/audit, inside `<Route path="/admin"
// nest>`). Its one outward link therefore has to name an address that is the
// same under both — which is what studioRoute() is for.
const listTeamAudit = vi.fn();
const listAdminAudit = vi.fn();

vi.mock("@/api/audit", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/audit")>();
  return {
    ...actual,
    listTeamAudit: (...a: unknown[]) => listTeamAudit(...a),
    listAdminAudit: (...a: unknown[]) => listAdminAudit(...a),
  };
});

const ORGS: OrgTreeView[] = [
  {
    org_id: "org-1",
    org_name: "Org One",
    org_slug: "org-one",
    org_role: "owner",
    teams: [{ team_id: "team-1", team_name: "Team One", team_slug: "team-one", role: "owner" }],
  },
];

vi.mock("@/auth/AuthContext", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/auth/AuthContext")>();
  return { ...actual, useMaybeAuth: () => ({ orgs: ORGS }) };
});

import AuditTab from "./AuditTab";
import { ADMIN_BASE } from "@/views/admin/adminRoutes";

const EMPTY = { events: [], next_offset: 0 };

beforeEach(() => {
  listTeamAudit.mockReset().mockResolvedValue(EMPTY);
  listAdminAudit.mockReset().mockResolvedValue(EMPTY);
});
afterEach(cleanup);

/** Renders the team-scope tab under `base`, optionally inside the console nest. */
function mountTeamScope(browserPath: string, nested: boolean) {
  const loc = memoryLocation({ path: browserPath, record: true });
  const tab = <AuditTab teamID="team-1" canManage />;
  render(
    <Router hook={loc.hook} base="/studio">
      {nested ? <Route path={ADMIN_BASE} nest>{tab}</Route> : tab}
    </Router>,
  );
  return loc;
}

async function clickOrgAuditLink() {
  fireEvent.click(await screen.findByRole("button", { name: "organization audit log" }));
}

describe("AuditTab — the org cross-link", () => {
  it("leads to the org's audit log from the team page", async () => {
    const loc = mountTeamScope("/studio/teams/team-1", false);
    await clickOrgAuditLink();
    expect(loc.history[loc.history.length - 1]).toBe("/studio/orgs/org-1?tab=audit");
  });

  // The same component inside the console's nested router: a plain
  // "/orgs/org-1" would resolve to /studio/admin/orgs/org-1 — a blank page.
  it("names the same address when the tab is mounted inside the /admin nest", async () => {
    const loc = mountTeamScope("/studio/admin/audit", true);
    await clickOrgAuditLink();
    expect(loc.history[loc.history.length - 1]).toBe("/studio/orgs/org-1?tab=audit");
  });
});

describe("AuditTab — platform scope", () => {
  // What the console actually mounts (AuditAdminPage): platform scope offers
  // no org cross-link at all, so the console cannot reach that branch.
  it("offers no org cross-link, and reads the platform feed", async () => {
    const loc = memoryLocation({ path: "/studio/admin/audit", record: true });
    render(
      <Router hook={loc.hook} base="/studio">
        <Route path={ADMIN_BASE} nest>
          <AuditTab platform canManage />
        </Route>
      </Router>,
    );
    expect(await screen.findByText(/Platform scope/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "organization audit log" })).toBeNull();
    expect(listAdminAudit).toHaveBeenCalled();
    expect(listTeamAudit).not.toHaveBeenCalled();
  });
});
