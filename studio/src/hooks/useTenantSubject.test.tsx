// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { OrgTreeView, MembershipView } from "@/api/auth";

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

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => identity,
  hasOrgRole: () => false,
}));

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
    getOrg.mockRejectedValue(new Error("403 forbidden"));
    const { result } = renderHook(() => useOrgSubject("o9"), { wrapper });

    await waitFor(() => expect(result.current.denied).toBe(true));
    expect(result.current.subject).toBeNull();
    // And it was not "denied" while the request was still in flight —
    // that state is what used to render as "not a member" before anything
    // had been asked.
    expect(result.current.loading).toBe(false);
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

  it("falls back to the server for a team that is not in the tree", async () => {
    getTeam.mockResolvedValue({
      id: "t9",
      name: "Other",
      slug: "other",
      status: "suspended",
    });
    const { result } = renderHook(() => useTeamSubject("t9"), { wrapper });

    await waitFor(() => expect(result.current.subject).not.toBeNull());
    expect(result.current.subject?.name).toBe("Other");
    expect(result.current.subject?.status).toBe("suspended");
    // The team row carries no org id, and saying so beats inventing one.
    expect(result.current.subject?.orgID).toBeNull();
  });
});

describe("useCanManageTeam", () => {
  it("uses the ACTIVE team's role when no team is named", () => {
    identity.activeRole = "admin";
    identity.activeTeamID = "t1";
    const { result } = renderHook(() => useCanManageTeam(), { wrapper });
    expect(result.current).toBe(true);
  });

  // The other half of the class: an org admin opening a team of their org
  // that is not the active one used to get canManage=false — no controls,
  // though the server accepts their writes.
  it("reads the named team's role, not the active one", () => {
    identity.activeRole = "viewer";
    identity.activeTeamID = "t1";
    identity.orgs = [
      {
        org_id: "o1",
        org_name: "SDPC",
        org_slug: "sdpc",
        org_role: "admin",
        teams: [
          { team_id: "t1", team_name: "One", team_slug: "one", role: "viewer" },
          // buildOrgTree gives an org admin an implied admin on every team.
          { team_id: "t2", team_name: "Two", team_slug: "two", role: "admin" },
        ],
      },
    ];
    expect(renderHook(() => useCanManageTeam("t2"), { wrapper }).result.current).toBe(
      true,
    );
    // And the active team's own role still governs itself — a viewer on t1
    // must not inherit t2's answer.
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
