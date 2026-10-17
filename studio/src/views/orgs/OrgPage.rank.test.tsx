// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { OrgTreeView } from "@/api/auth";
import type { OrgMemberView } from "@/api/orgMembers";

// The gate on this page is a MIRROR of the server's: the controls it leaves
// enabled are the writes requireRankOverMember accepts. What the mirror reads
// is the whole question — the roster row, as the guard does, not the identity
// tree, which is fetched once at sign-in and no roster write refreshes.
const identity = {
  orgs: [] as OrgTreeView[],
  teams: [] as unknown[],
  activeOrgRole: "" as string,
  activeRole: "" as string,
  activeTeamID: "",
  user: {
    id: "me",
    email: "me@example.org",
    status: "active",
    is_super_admin: false,
  } as { id: string; email: string; status: string; is_super_admin: boolean },
};

// Only `useAuth` is replaced: `hasOrgRole` keeps its real implementation,
// since canManage is built from it and a stub would make the org-admin arm
// untestable while the suite stayed green.
vi.mock("@/auth/AuthContext", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/auth/AuthContext")>();
  return { ...actual, useAuth: () => identity };
});

const listOrgMembers = vi.fn<(orgID: string) => Promise<OrgMemberView[]>>();
const listOrgInvitations = vi.fn(async () => [] as unknown[]);
vi.mock("@/api/orgMembers", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgMembers")>();
  return {
    ...actual,
    listOrgMembers: (id: string) => listOrgMembers(id),
    listOrgInvitations: () => listOrgInvitations(),
  };
});
const getOrg = vi.fn();
vi.mock("@/api/orgs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgs")>();
  return { ...actual, getOrg: (id: string) => getOrg(id) };
});
vi.mock("@/api/orgGovernance", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgGovernance")>();
  return { ...actual, listOrgTeamSummaries: async () => [] };
});
vi.mock("@/api/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/admin")>();
  return { ...actual, listAdminUsers: async () => ({ users: [], total: 0 }) };
});

import OrgPage from "./OrgPage";

const ORG_ID = "o1";

function treeOrg(role: string): OrgTreeView {
  return {
    org_id: ORG_ID,
    org_name: "SDPC",
    org_slug: "sdpc",
    org_role: role as OrgTreeView["org_role"],
    teams: [],
  };
}

function member(userID: string, role: string): OrgMemberView {
  return {
    user_id: userID,
    email: `${userID}@example.org`,
    name: userID,
    role: role as OrgMemberView["role"],
  };
}

function mount() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  const loc = memoryLocation({ path: `/studio/orgs/${ORG_ID}` });
  const rendered = render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base="/studio">
        <Route path="/orgs/:id">
          <OrgPage />
        </Route>
      </Router>
    </QueryClientProvider>,
  );
  // The client is returned so a test can make the page REFETCH — the only way
  // to reach the state where the roster holds rows and its last read failed.
  return { ...rendered, qc };
}

/** A refetch of the roster, awaited, with its rejection left to the page. */
async function refetchRoster(qc: QueryClient): Promise<void> {
  await act(async () => {
    await qc.refetchQueries({ queryKey: ["org-members", ORG_ID] }).catch(() => {});
  });
}

/** The role select of one row, by the label a screen reader reads. */
function roleSelect(userID: string): HTMLSelectElement {
  return screen.getByLabelText(
    `Org role for ${userID}@example.org`,
  ) as HTMLSelectElement;
}

/** The row's Remove button, found through the row rather than by name — every
 *  row has one, and the one under test is the one beside this member. */
function removeButton(userID: string): HTMLButtonElement {
  const tr = screen.getByText(`${userID}@example.org`).closest("tr");
  const btn = tr?.querySelector("button");
  if (!btn) throw new Error(`no Remove button on the row of ${userID}`);
  return btn as HTMLButtonElement;
}

/** The reason a control announces, reached the way a screen reader reaches
 *  it: through `aria-describedby`, never by searching for the text — a
 *  wording regression must fail on the claim, not on the locator. */
function announcedReason(control: Element): string {
  const ref = control.getAttribute("aria-describedby");
  if (!ref) throw new Error("the control announces no reason");
  // `aria-describedby` is a LIST of ids, and a reader announces every one of
  // them in order: resolve them all, and refuse a dangling one.
  const ids = ref.split(/\s+/).filter((id) => id !== "");
  return ids
    .map((id) => {
      const hint = globalThis.document.getElementById(id);
      if (!hint) throw new Error(`the control describes a missing element ${id}`);
      return hint.textContent ?? "";
    })
    .join(" ");
}

beforeEach(() => {
  identity.orgs = [];
  identity.user = {
    id: "me",
    email: "me@example.org",
    status: "active",
    is_super_admin: false,
  };
  listOrgMembers.mockReset().mockResolvedValue([]);
  listOrgInvitations.mockReset().mockResolvedValue([]);
  getOrg.mockReset();
});
afterEach(cleanup);

describe("OrgPage · the roster's rank gate", () => {
  it("leaves every control enabled for a platform admin who is not a member", async () => {
    // No org row at all, so the ladder has nothing to place them on — the
    // exemption is the whole reason they can act here.
    identity.user = { ...identity.user, is_super_admin: true };
    getOrg.mockResolvedValue({ id: ORG_ID, name: "SDPC", slug: "sdpc", personal: false });
    listOrgMembers.mockResolvedValue([member("boss", "owner")]);
    mount();

    expect((await screen.findByLabelText("Org role for boss@example.org")).hasAttribute(
      "disabled",
    )).toBe(false);
    expect(removeButton("boss").disabled).toBe(false);
    expect(roleSelect("boss").getAttribute("aria-describedby")).toBeNull();
  });

  it("disables a row above the viewer, and says who writes it", async () => {
    identity.orgs = [treeOrg("admin")];
    listOrgMembers.mockResolvedValue([member("me", "admin"), member("boss", "owner")]);
    mount();

    const select = (await screen.findByLabelText(
      "Org role for boss@example.org",
    )) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
    expect(removeButton("boss").disabled).toBe(true);
    // One reason, named by both controls it explains — and it names a remedy
    // that can make THIS write.
    const reason = announcedReason(select);
    expect(reason).toContain("a member of this org who already holds owner");
    expect(reason).toContain("platform admin");
    expect(announcedReason(removeButton("boss"))).toBe(reason);
    // A disabled select shows the role the row holds, not the first option of
    // a ladder it is not on.
    expect(select.value).toBe("owner");

    // Their own row stays theirs: equal rank passes.
    expect(roleSelect("me").disabled).toBe(false);
    expect(removeButton("me").disabled).toBe(false);
  });

  it("takes the viewer's standing from the roster row, not the identity tree", async () => {
    // The tree says admin — it was fetched at sign-in and nothing refreshes
    // it. The roster says owner, which is the row the server's guard reads.
    identity.orgs = [treeOrg("admin")];
    listOrgMembers.mockResolvedValue([member("me", "owner"), member("boss", "owner")]);
    mount();

    expect((await screen.findByLabelText("Org role for boss@example.org")).hasAttribute(
      "disabled",
    )).toBe(false);
    expect(removeButton("boss").disabled).toBe(false);
    // And the ladder is offered up to the standing the roster carries.
    expect(Array.from(roleSelect("boss").options).map((o) => o.value)).toEqual([
      "member",
      "admin",
      "owner",
    ]);
  });

  it("renders no control after a self-demotion the roster already shows", async () => {
    // The dual: signed in as owner, since demoted to member on their own row.
    // canManageOrg now refuses this author on EVERY row, so what is owed is
    // not a disabled select — it is no write control at all, and a roster
    // that still reads.
    identity.orgs = [treeOrg("owner")];
    listOrgMembers.mockResolvedValue([member("me", "member"), member("boss", "owner")]);
    mount();

    await screen.findByText("boss@example.org");
    await waitFor(() => expect(listOrgMembers).toHaveBeenCalled());
    expect(screen.queryAllByLabelText("Org role for boss@example.org")).toHaveLength(0);
    expect(screen.queryAllByRole("button", { name: "Remove" })).toHaveLength(0);
    expect(screen.queryByText("Invite a member")).toBeNull();
    expect(screen.getByText("owner")).toBeTruthy();
  });

  it("renders the controls a promotion the roster already shows", async () => {
    // The mirror image: the tree says member, the roster says owner — the
    // row canManageOrg reads. Read from the tree, this surface offered
    // nothing at all.
    identity.orgs = [treeOrg("member")];
    listOrgMembers.mockResolvedValue([member("me", "owner"), member("peer", "member")]);
    mount();

    const select = (await screen.findByLabelText(
      "Org role for peer@example.org",
    )) as HTMLSelectElement;
    expect(select.disabled).toBe(false);
    expect(removeButton("peer").disabled).toBe(false);
    // And the whole ladder, which is what an owner may write.
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      "member",
      "admin",
      "owner",
    ]);
  });

  it("offers nothing while the roster the gate reads is in flight", async () => {
    // The tree says admin; nothing is offered on its word alone.
    identity.orgs = [treeOrg("admin")];
    let release: (v: OrgMemberView[]) => void = () => {};
    listOrgMembers.mockImplementation(
      () => new Promise<OrgMemberView[]>((res) => (release = res)),
    );
    mount();

    await waitFor(() => expect(listOrgMembers).toHaveBeenCalled());
    expect(screen.queryByText("Invite a member")).toBeNull();

    release([member("me", "admin"), member("peer", "member")]);
    expect(
      (await screen.findByLabelText("Org role for peer@example.org")).hasAttribute(
        "disabled",
      ),
    ).toBe(false);
    expect(screen.getByText("Invite a member")).toBeTruthy();
  });

  it("renders nothing to write for a viewer the roster answered and dropped", async () => {
    // The state the gate's identity-tree arm used to serve: the tree still
    // says admin — fetched at sign-in, and no roster write refreshes it —
    // while the roster has ANSWERED and carries no row for the viewer, their
    // org membership having just been removed. The server answers 403 "org
    // admin or owner required" to every write on this page AND to the mint
    // beside it (measured through the minting handlers: the 18 org-route
    // cells at own_org=none are all refused by the gate), so what is owed is
    // no write control at all — not a page that disables every row and offers
    // the whole ladder in the invitation picker above them.
    identity.orgs = [treeOrg("admin")];
    getOrg.mockResolvedValue({ id: ORG_ID, name: "SDPC", slug: "sdpc", personal: false });
    listOrgMembers.mockResolvedValue([member("boss", "owner"), member("peer", "member")]);
    mount();

    await screen.findByText("peer@example.org");
    await waitFor(() => expect(listOrgMembers).toHaveBeenCalled());
    expect(screen.queryByText("Invite a member")).toBeNull();
    expect(screen.queryByRole("button", { name: "Send invite" })).toBeNull();
    expect(screen.queryAllByLabelText("Org role for peer@example.org")).toHaveLength(0);
    expect(screen.queryAllByRole("button", { name: "Remove" })).toHaveLength(0);
    // The roster still reads, which is what this viewer may do here.
    expect(screen.getByText("member")).toBeTruthy();
  });

  it("stops deciding from rows a failed refetch could not refresh", async () => {
    // react-query keeps `data` through a failed refetch and sets `error`: the
    // one state where the order of the two decides anything. The rows are
    // stale and the page has no current read of them, so the gate they feed
    // closes — a control enabled from them would be enabled for good.
    identity.orgs = [treeOrg("admin")];
    getOrg.mockResolvedValue({ id: ORG_ID, name: "SDPC", slug: "sdpc", personal: false });
    listOrgMembers
      .mockResolvedValueOnce([member("me", "admin"), member("peer", "member")])
      .mockRejectedValue(new Error("org roster: 503 refetch"));
    const { qc } = mount();

    expect(
      (await screen.findByLabelText("Org role for peer@example.org")).hasAttribute(
        "disabled",
      ),
    ).toBe(false);

    await refetchRoster(qc);

    await waitFor(() => expect(screen.queryByText("Invite a member")).toBeNull());
    expect(screen.queryAllByLabelText("Org role for peer@example.org")).toHaveLength(0);
    // The stale rows are still SHOWN — losing them would hide the roster as
    // well as the controls — and the failure is reported.
    expect(screen.getByText("peer@example.org")).toBeTruthy();
    expect(screen.getByText(/org roster: 503 refetch/)).toBeTruthy();
  });

  it("offers no role above the viewer's own standing, and says so", async () => {
    identity.orgs = [treeOrg("admin")];
    listOrgMembers.mockResolvedValue([member("me", "admin"), member("peer", "member")]);
    mount();

    await screen.findByLabelText("Org role for peer@example.org");
    // `owner` is the write requireOrgStandingToWriteOrgRole always refuses an
    // org admin — on any row, their own included.
    expect(Array.from(roleSelect("peer").options).map((o) => o.value)).toEqual([
      "member",
      "admin",
    ]);
    expect(Array.from(roleSelect("me").options).map((o) => o.value)).toEqual([
      "member",
      "admin",
    ]);
    expect(
      screen.getByText(/Roles above your own are not offered \(owner\)/),
    ).toBeTruthy();
  });
});

describe("OrgPage · two Members sections in one document", () => {
  it("carries no duplicate id", async () => {
    // Every id this section renders is unique to its instance, the row
    // reasons included: `getElementById` resolves the FIRST, so a duplicated
    // reason id makes the second section's controls describe the first's text.
    identity.orgs = [treeOrg("admin")];
    getOrg.mockResolvedValue({ id: ORG_ID, name: "SDPC", slug: "sdpc", personal: false });
    listOrgMembers.mockResolvedValue([member("me", "admin"), member("boss", "owner")]);
    mount();
    await screen.findByLabelText("Org role for boss@example.org");
    mount();
    await waitFor(() =>
      expect(screen.getAllByLabelText("Org role for boss@example.org")).toHaveLength(2),
    );

    const all = Array.from(globalThis.document.querySelectorAll("[id]")).map((e) => e.id);
    expect(all.filter((id, i) => all.indexOf(id) !== i)).toEqual([]);
    // Not vacuous: the refused row's reason is rendered by both sections, and
    // the two ids differ.
    const rowReasons = all.filter((id) => id.endsWith("-org-member-rank-boss"));
    expect(rowReasons).toHaveLength(2);
    expect(new Set(rowReasons).size).toBe(2);
  });
});
