// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { OrgTreeView, MembershipView } from "@/api/auth";
import { ApiError } from "@/api/client";

// One mutable identity, shared by both hooks under test and by
// useCanManageTeam — all three read the same tree, which is the point:
// "may I see it" and "may I manage it" must not answer from two different
// pictures of the caller.
const identity = {
  orgs: [] as OrgTreeView[],
  teams: [] as MembershipView[],
  activeOrgRole: "" as string,
  activeRole: "" as string,
  activeTeamID: "",
  user: { id: "me", email: "me@example.org", status: "active", is_super_admin: false },
};

// Only `useAuth` is replaced. `hasOrgRole` keeps its REAL implementation:
// useCanManageTeam mirrors the server's three-armed `canManageTeam`, and a
// stub that answered `false` would certify nothing — it would make the
// org-admin arm untestable while the suite stayed green.
vi.mock("@/auth/AuthContext", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/auth/AuthContext")>();
  return { ...actual, useAuth: () => identity };
});

const getOrg = vi.fn();
const getTeam = vi.fn();
vi.mock("@/api/orgs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgs")>();
  return {
    ...actual,
    getOrg: (id: string) => getOrg(id),
    getTeam: (id: string) => getTeam(id),
  };
});

import { useCanManageTeam } from "./useCanManageTeam";
import { useOrgSubject, useTeamSubject } from "./useTenantSubject";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  identity.orgs = [];
  identity.teams = [];
  identity.activeOrgRole = "";
  identity.activeRole = "";
  identity.activeTeamID = "";
  identity.user = {
    id: "me",
    email: "me@example.org",
    status: "active",
    is_super_admin: false,
  };
  getOrg.mockReset();
  getTeam.mockReset();
});
afterEach(cleanup);

describe("useOrgSubject", () => {
  it("answers from the identity tree without asking the server", async () => {
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "SDPC",
        org_slug: "sdpc",
        org_role: "member",
        teams: [],
      },
    ];
    const { result } = renderHook(() => useOrgSubject("o1"), { wrapper });

    expect(result.current.subject?.name).toBe("SDPC");
    expect(result.current.subject?.role).toBe("member");
    expect(result.current.subject?.isMember).toBe(true);
    // A member pays no request: the tree already carries the answer AND
    // their role, which the API row does not.
    expect(getOrg).not.toHaveBeenCalled();
  });

  // The defect this whole hook exists for: /orgs/:id used to read "You are
  // not a member of this organization" for a super-admin, although every
  // API behind that page would have answered.
  it("falls back to the server for an org that is not in the tree", async () => {
    getOrg.mockResolvedValue({
      id: "o9",
      name: "Other",
      slug: "other",
      status: "active",
    });
    const { result } = renderHook(() => useOrgSubject("o9"), { wrapper });

    await waitFor(() => expect(result.current.subject).not.toBeNull());
    expect(result.current.subject?.name).toBe("Other");
    // Honest about what it does NOT know: no membership, so no role.
    expect(result.current.subject?.isMember).toBe(false);
    expect(result.current.subject?.role).toBeNull();
    expect(getOrg).toHaveBeenCalledWith("o9");
  });

  it("reports denial rather than a silent empty subject", async () => {
    getOrg.mockRejectedValue(new ApiError(403, "API error 403: forbidden"));
    const { result } = renderHook(() => useOrgSubject("o9"), { wrapper });

    await waitFor(() => expect(result.current.denied).toBe(true));
    expect(result.current.subject).toBeNull();
    expect(result.current.notFound).toBe(false);
    expect(result.current.error).toBeNull();
    // And it was not "denied" while the request was still in flight —
    // that state is what used to render as "not a member" before anything
    // had been asked.
    expect(result.current.loading).toBe(false);
  });

  // A super-admin PASSES canViewOrg, so an org that simply does not exist
  // comes back 404. Calling that "denied" tells them they lack an access
  // they hold, and the page's own not-found branch becomes unreachable.
  it("separates a 404 from a refusal", async () => {
    getOrg.mockRejectedValue(new ApiError(404, "API error 404: identity: not found"));
    const { result } = renderHook(() => useOrgSubject("o9"), { wrapper });

    await waitFor(() => expect(result.current.notFound).toBe(true));
    expect(result.current.denied).toBe(false);
    expect(result.current.error).toBeNull();
  });

  // And an outage is neither. Dressing a 500 as a permission message buries
  // the cause — the repo's no-silent-fallback rule, applied to a read.
  it("surfaces a transient failure as an error, not as a refusal", async () => {
    getOrg.mockRejectedValue(new ApiError(500, "API error 500: upstream exploded"));
    const { result } = renderHook(() => useOrgSubject("o9"), { wrapper });

    await waitFor(() => expect(result.current.error).toBeTruthy());
    expect(result.current.denied).toBe(false);
    expect(result.current.notFound).toBe(false);
    expect(String(result.current.error)).toContain("exploded");
  });
});

describe("useTeamSubject", () => {
  it("carries the owning org from the tree", () => {
    identity.teams = [
      { team_id: "t1", team_name: "PIC", team_slug: "pic", role: "member" },
    ];
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "SDPC",
        org_slug: "sdpc",
        org_role: "member",
        teams: [{ team_id: "t1", team_name: "PIC", team_slug: "pic", role: "member" }],
      },
    ];
    const { result } = renderHook(() => useTeamSubject("t1"), { wrapper });

    expect(result.current.subject?.orgID).toBe("o1");
    expect(getTeam).not.toHaveBeenCalled();
  });

  // `useAuth().teams` is `activeOrg?.teams` — the ACTIVE org's teams only.
  // Resolving from it would make a team the caller genuinely belongs to, in
  // another org, look like a team they have no grant on.
  it("finds a grant in a NON-ACTIVE org", () => {
    identity.teams = []; // the active org's list, which does not contain t2
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "Active",
        org_slug: "active",
        org_role: "member",
        teams: [{ team_id: "t1", team_name: "One", team_slug: "one", role: "member" }],
      },
      {
        org_id: "o2",
        org_name: "Other",
        org_slug: "other",
        org_role: "member",
        teams: [{ team_id: "t2", team_name: "Two", team_slug: "two", role: "admin" }],
      },
    ];
    const { result } = renderHook(() => useTeamSubject("t2"), { wrapper });

    expect(result.current.subject?.isMember).toBe(true);
    expect(result.current.subject?.role).toBe("admin");
    expect(result.current.subject?.orgID).toBe("o2");
    expect(result.current.subject?.orgName).toBe("Other");
    // And it did not pay for a request it did not need.
    expect(getTeam).not.toHaveBeenCalled();
  });

  it("falls back to the server for a team that is not in the tree", async () => {
    getTeam.mockResolvedValue({
      id: "t9",
      org_id: "o9",
      name: "Other",
      slug: "other",
      status: "suspended",
    });
    const { result } = renderHook(() => useTeamSubject("t9"), { wrapper });

    await waitFor(() => expect(result.current.subject).not.toBeNull());
    expect(result.current.subject?.name).toBe("Other");
    expect(result.current.subject?.status).toBe("suspended");
    // The team row carries its own org id, so the parent resolves for a
    // non-member too — which is what lets a super-admin ACT on the team
    // rather than only look at it.
    expect(result.current.subject?.orgID).toBe("o9");
    // The name is not on that row, and saying so beats inventing one.
    expect(result.current.subject?.orgName).toBeNull();
  });
});

describe("useCanManageTeam", () => {
  it("reads the grant on the named team", () => {
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "SDPC",
        org_slug: "sdpc",
        org_role: "member",
        teams: [{ team_id: "t1", team_name: "One", team_slug: "one", role: "admin" }],
      },
    ];
    const { result } = renderHook(() => useCanManageTeam("t1"), { wrapper });
    expect(result.current).toBe(true);
  });

  // The other half of the class: an org admin opening a team of their org
  // that is not the active one used to get canManage=false — no controls,
  // though the server accepts their writes.
  it("reads the named team's grant, not the active team's role", () => {
    identity.activeRole = "viewer";
    identity.activeTeamID = "t1";
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "Plain",
        org_slug: "plain",
        org_role: "member",
        teams: [
          { team_id: "t1", team_name: "One", team_slug: "one", role: "viewer" },
          { team_id: "t2", team_name: "Two", team_slug: "two", role: "admin" },
        ],
      },
    ];
    expect(renderHook(() => useCanManageTeam("t2"), { wrapper }).result.current).toBe(
      true,
    );
    // A viewer on t1 must not inherit t2's answer.
    expect(renderHook(() => useCanManageTeam("t1"), { wrapper }).result.current).toBe(
      false,
    );
  });

  // The server's canManageTeam has THREE arms: super-admin, team admin/owner,
  // OR admin/owner of the team's org. The third looks redundant because
  // buildOrgTree synthesises `admin` for an org admin — but it does that ONLY
  // where no explicit grant exists. An org admin who also holds an explicit
  // LOWER row on the team therefore arrives as `viewer`, and reading the team
  // role alone hides every control the server would accept.
  it("honours the org-admin arm when an explicit lower team grant exists", () => {
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "SDPC",
        org_slug: "sdpc",
        org_role: "admin",
        teams: [{ team_id: "t1", team_name: "One", team_slug: "one", role: "viewer" }],
      },
    ];
    expect(renderHook(() => useCanManageTeam("t1"), { wrapper }).result.current).toBe(
      true,
    );
  });

  it("does not widen: a plain org member with a viewer grant stays refused", () => {
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "SDPC",
        org_slug: "sdpc",
        org_role: "member",
        teams: [{ team_id: "t1", team_name: "One", team_slug: "one", role: "viewer" }],
      },
    ];
    expect(renderHook(() => useCanManageTeam("t1"), { wrapper }).result.current).toBe(
      false,
    );
  });

  it("refuses a team absent from the tree rather than reusing the active role", () => {
    identity.activeRole = "owner";
    identity.activeTeamID = "t1";
    const { result } = renderHook(() => useCanManageTeam("t-unknown"), { wrapper });
    expect(result.current).toBe(false);
  });

  it("lets a super-admin manage any team", () => {
    identity.user = { ...identity.user, is_super_admin: true };
    identity.activeRole = "viewer";
    const { result } = renderHook(() => useCanManageTeam("t-unknown"), { wrapper });
    expect(result.current).toBe(true);
  });
});
