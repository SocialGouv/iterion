// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { AdminUserDetail } from "@/api/admin";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

const getAdminUser = vi.fn<(id: string) => Promise<AdminUserDetail>>();
const listOrgs = vi.fn(async () => [] as unknown[]);
const listAdminOrgTeams = vi.fn(async () => [] as unknown[]);

vi.mock("@/api/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/admin")>();
  return { ...actual, getAdminUser: (id: string) => getAdminUser(id) };
});
vi.mock("@/api/orgs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgs")>();
  return {
    ...actual,
    listOrgs: () => listOrgs(),
    listAdminOrgTeams: () => listAdminOrgTeams(),
  };
});
import UserDrawer from "./UserDrawer";

function detail(over: Partial<AdminUserDetail> = {}): AdminUserDetail {
  return {
    user: {
      id: "u1",
      email: "someone@externes.example.org",
      status: "active",
      is_super_admin: false,
      created_at: "2026-09-17T09:00:00Z",
    },
    has_password: true,
    orgs: [],
    teams: [],
    sso_links: [],
    ...over,
  };
}

// The real wouter, in the drawer's real place: the /admin console is mounted
// inside <Router base="/studio"> as a nested route, so a plain "/orgs/<id>"
// href would resolve against /studio/admin, not /studio. A stubbed <Link>
// renders the href it is handed and would never show that.
function renderDrawer() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  const loc = memoryLocation({ path: "/studio/admin/users" });
  return render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base="/studio">
        <Route path="/admin" nest>
          <UserDrawer userID="u1" onClose={() => {}} />
        </Route>
      </Router>
    </QueryClientProvider>,
  );
}

// Radix renders the dialog in a portal, outside render()'s container, so the
// drawer's own subtree is reached by role and not by the render result.
function mountDrawer(): HTMLElement {
  renderDrawer();
  return screen.getByRole("dialog");
}

setupMatchMedia();

beforeEach(() => {
  getAdminUser.mockReset();
  listOrgs.mockReset().mockResolvedValue([]);
  listAdminOrgTeams.mockReset().mockResolvedValue([]);
});
afterEach(cleanup);

describe("UserDrawer", () => {
  // The account this whole console was built for: a GitHub login admitted
  // outside the SSO org allow-list. Active, an SSO link, no password, an
  // empty roster. Each fact alone reads as a broken deployment; the drawer
  // has to put them together, because that is the diagnosis an operator
  // spent a session arriving at by hand.
  it("names the submitter account instead of showing four unrelated facts", async () => {
    getAdminUser.mockResolvedValue(
      detail({
        has_password: false,
        sso_links: [
          { provider: "github", subject: "1234567", email: "someone@externes.example.org" },
        ],
      }),
    );
    renderDrawer();

    expect(await screen.findByText(/belongs to nothing/i)).toBeTruthy();
    // The two halves of "no way in" are both stated, not implied.
    expect(screen.getByText(/cannot sign in with a password/i)).toBeTruthy();
    expect(screen.getByText("github")).toBeTruthy();
    expect(screen.getByText("1234567")).toBeTruthy();
    expect(screen.getByText(/never/i)).toBeTruthy();
  });

  // The banner is a diagnosis, not decoration: an account that IS placed
  // must not carry it, or it names nothing.
  it("does not warn about a placed account", async () => {
    getAdminUser.mockResolvedValue(
      detail({
        orgs: [{ org_id: "o1", org_name: "SDPC", org_slug: "sdpc", role: "member" }],
        teams: [
          {
            team_id: "t1",
            team_name: "PIC",
            team_slug: "pic",
            org_id: "o1",
            org_name: "SDPC",
            role: "admin",
          },
        ],
      }),
    );
    renderDrawer();

    // Both rows link to the tenant they name, which is the drawer's other
    // job: an account's memberships are a way IN to those pages. The target
    // is the studio's own /orgs|/teams page — the console's /admin base must
    // not leak into it.
    expect(
      (await screen.findByRole("link", { name: "SDPC" })).getAttribute("href"),
    ).toBe("/studio/orgs/o1");
    expect(screen.getByRole("link", { name: "PIC" }).getAttribute("href")).toBe(
      "/studio/teams/t1",
    );
    expect(screen.queryByText(/belongs to nothing/i)).toBeNull();
  });

  // A team grant with no org membership behind it is drift the invariant
  // says cannot happen. Rendering it as an ordinary row would hide the one
  // case worth opening this page for.
  it("flags a team grant with no org membership behind it", async () => {
    getAdminUser.mockResolvedValue(
      detail({
        orgs: [],
        teams: [
          {
            team_id: "t9",
            team_name: "Orphan",
            team_slug: "orphan",
            org_id: "o9",
            org_name: "Gone",
            role: "admin",
            orphan_grant: true,
          },
        ],
      }),
    );
    renderDrawer();

    expect(await screen.findByText(/this grant should not exist/i)).toBeTruthy();
  });

  // The same missing boundary under a role that is mirrored up to no org
  // membership at all (config_editor, ADR-078): the row still shows — an
  // operator looking for the access an account holds needs it — but it is
  // not the drift to repair.
  it("reads an off-ladder grant's missing org membership as expected", async () => {
    getAdminUser.mockResolvedValue(
      detail({
        orgs: [],
        teams: [
          {
            team_id: "t9",
            team_name: "Configs",
            team_slug: "configs",
            org_id: "o9",
            org_name: "Nine",
            role: "config_editor",
            orphan_grant: true,
            off_ladder: true,
          },
        ],
      }),
    );
    renderDrawer();

    expect(
      await screen.findByText(/expected for this team-only role/i),
    ).toBeTruthy();
    expect(screen.queryByText(/this grant should not exist/i)).toBeNull();
  });

  // The server refuses a team grant for a user outside the team's org
  // (422). Rather than surface that refusal, the picker cannot express it:
  // with no org membership there is no team to pick, and the drawer says
  // which step comes first.
  it("offers no team picker until the account has an org", async () => {
    getAdminUser.mockResolvedValue(detail());
    renderDrawer();

    expect(
      await screen.findByText(/Add this account to an organization first/i),
    ).toBeTruthy();
    expect(screen.queryByLabelText("Team")).toBeNull();
    // And it never asked for teams it could not offer.
    await waitFor(() => expect(listAdminOrgTeams).not.toHaveBeenCalled());
  });

  it("offers the team picker once the account belongs to an org", async () => {
    getAdminUser.mockResolvedValue(
      detail({ orgs: [{ org_id: "o1", org_name: "SDPC", org_slug: "sdpc", role: "member" }] }),
    );
    renderDrawer();

    expect(await screen.findByLabelText("Team")).toBeTruthy();
    expect(screen.queryByText(/Add this account to an organization first/i)).toBeNull();
  });
});

// Two of each, and the names deliberately COLLIDE: a fixture with distinct
// display names cannot show whether a repeated control's name is unique. Two
// organizations share the display name "Platform"; the same team name — and
// slug — exists in both, which is legal because a team name is per-org.
const PLACED = detail({
  orgs: [
    { org_id: "o1", org_name: "Platform", org_slug: "sdpc", role: "member" },
    { org_id: "o2", org_name: "Platform", org_slug: "dnum", role: "owner" },
  ],
  teams: [
    { team_id: "t1", team_name: "Core", team_slug: "core", org_id: "o1", org_name: "SDPC", role: "admin" },
    { team_id: "t2", team_name: "Core", team_slug: "core", org_id: "o2", org_name: "DNUM", role: "member" },
  ],
  sso_links: [{ provider: "github", subject: "1234567", email: "someone@externes.example.org" }],
});

describe("UserDrawer — repeated row controls name the row they act on", () => {
  // Every membership row carries the same verb. Without the row in the name,
  // a screen-reader user hears "Remove, Remove" and has to count table rows
  // to know which organization the second one revokes.
  it("qualifies each membership's destructive control by its tenant", async () => {
    getAdminUser.mockResolvedValue(PLACED);
    mountDrawer();

    expect(await screen.findByRole("button", { name: "Remove from sdpc" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove from dnum" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Revoke core in SDPC" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Revoke core in DNUM" })).toBeTruthy();
    // The visible words are untouched: the name CONTAINS them (SC 2.5.3).
    for (const [visible, count] of [
      ["Remove", 2],
      ["Revoke", 2],
    ] as const) {
      expect(screen.getAllByText(visible)).toHaveLength(count);
    }
  });
});

describe("a11y / UserDrawer", () => {
  it("has no violation with both rosters loaded", async () => {
    getAdminUser.mockResolvedValue(PLACED);
    const dialog = mountDrawer();
    await screen.findByRole("table", { name: "Organizations this account belongs to" });
    await expectNoViolationsStrict(dialog, "UserDrawer");
  });

  it("has no violation on the unplaced account the console exists for", async () => {
    getAdminUser.mockResolvedValue(detail({ has_password: false }));
    const dialog = mountDrawer();
    await screen.findByText(/belongs to nothing/i);
    await expectNoViolationsStrict(dialog, "UserDrawer (unplaced)");
  });

  it("has no violation while the account is still loading", async () => {
    let release: (d: AdminUserDetail) => void = () => {};
    getAdminUser.mockReturnValue(
      new Promise<AdminUserDetail>((resolve) => {
        release = resolve;
      }),
    );
    const dialog = mountDrawer();
    // The slate is a named live region, not visible text (Spinner carries the
    // label on role=status), so it is asserted the way it is announced.
    await screen.findByRole("status", { name: "Loading account…" });
    await expectNoViolationsStrict(dialog, "UserDrawer (loading)");
    release(detail());
  });
});
