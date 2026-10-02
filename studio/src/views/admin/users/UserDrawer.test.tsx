// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { AdminUserDetail } from "@/api/admin";

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
vi.mock("wouter", () => ({
  Link: ({
    href,
    children,
    ...rest
  }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

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

function renderDrawer() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <UserDrawer userID="u1" onClose={() => {}} />
    </QueryClientProvider>,
  );
}

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
    // job: an account's memberships are a way IN to those pages.
    expect(
      (await screen.findByRole("link", { name: "SDPC" })).getAttribute("href"),
    ).toBe("/orgs/o1");
    expect(screen.getByRole("link", { name: "PIC" }).getAttribute("href")).toBe(
      "/teams/t1",
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
