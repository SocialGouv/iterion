// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { OrgTreeView } from "@/api/auth";
import type { TeamMemberView } from "@/api/byok";
import type { OrgMemberView } from "@/api/orgMembers";
// The error every non-2xx response throws, so a refusal a test hands the page
// carries the status the page reads and not only a message.
import { apiErrorFrom } from "@/api/client";

// A team-scope write answers to TWO ladders — the parent org's, and the
// team's when the viewer's authority over the team is their own grant — and
// both read the roster rows the server's guard reads.
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

// `useAuth` only: useCanManageTeam and hasOrgRole keep their real
// implementations, so the org-admin arm of canManageTeam is really exercised.
vi.mock("@/auth/AuthContext", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/auth/AuthContext")>();
  return { ...actual, useAuth: () => identity };
});

const listTeamMembers = vi.fn<(teamID: string) => Promise<TeamMemberView[]>>();
const listInvitations = vi.fn(async () => [] as unknown[]);
vi.mock("@/api/byok", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/byok")>();
  return {
    ...actual,
    listTeamMembers: (id: string) => listTeamMembers(id),
    listInvitations: () => listInvitations(),
  };
});
const listOrgMembers = vi.fn<(orgID: string) => Promise<OrgMemberView[]>>();
vi.mock("@/api/orgMembers", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgMembers")>();
  return { ...actual, listOrgMembers: (id: string) => listOrgMembers(id) };
});
const getTeam = vi.fn();
vi.mock("@/api/orgs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/orgs")>();
  return { ...actual, getTeam: (id: string) => getTeam(id) };
});

import TeamPage from "./TeamPage";

const ORG_ID = "o1";
const TEAM_ID = "t1";

/** The identity tree: one org, one grant on the team under test. */
function tree(orgRole: string, teamRole: string): OrgTreeView[] {
  return [
    {
      org_id: ORG_ID,
      org_name: "SDPC",
      org_slug: "sdpc",
      org_role: orgRole as OrgTreeView["org_role"],
      teams: [
        {
          team_id: TEAM_ID,
          team_name: "PIC",
          team_slug: "pic",
          role: teamRole as OrgTreeView["teams"][number]["role"],
        },
      ],
    },
  ];
}

function teamMember(userID: string, role: string): TeamMemberView {
  return {
    user_id: userID,
    email: `${userID}@example.org`,
    name: userID,
    role: role as TeamMemberView["role"],
  };
}

function orgMember(userID: string, role: string): OrgMemberView {
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
  const loc = memoryLocation({ path: `/studio/teams/${TEAM_ID}` });
  const rendered = render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base="/studio">
        <Route path="/teams/:id">
          <TeamPage />
        </Route>
      </Router>
    </QueryClientProvider>,
  );
  // The client is returned so a test can make the page REFETCH the org
  // roster — the only way to reach the state where its rows are in hand and
  // its last read failed.
  return { ...rendered, qc };
}

/** A refetch of the org roster, awaited, with its rejection left to the page. */
async function refetchOrgRoster(qc: QueryClient): Promise<void> {
  await act(async () => {
    await qc.refetchQueries({ queryKey: ["org-members", ORG_ID] }).catch(() => {});
  });
}

/** The add-an-existing-account panel, when the page renders one. */
function candidatePanel(): HTMLElement | null {
  return screen.queryByText("Add someone who already has an account");
}

function roleSelect(userID: string): HTMLSelectElement {
  return screen.getByLabelText(`Role for ${userID}@example.org`) as HTMLSelectElement;
}

function removeButton(userID: string): HTMLButtonElement {
  const tr = screen.getByText(`${userID}@example.org`).closest("tr");
  const btn = tr?.querySelector("button");
  if (!btn) throw new Error(`no Remove button on the row of ${userID}`);
  return btn as HTMLButtonElement;
}

/** The invitation form's role select, reached through the form it belongs to:
 *  its control ids are unique to the form instance, and the page's other
 *  "Role" select belongs to the add-an-existing-account panel. */
function inviteForm(): HTMLFormElement | null {
  const send = screen.queryByRole("button", { name: "Send invite" });
  return (send?.closest("form") as HTMLFormElement | null) ?? null;
}

function inviteRoleSelect(): HTMLSelectElement {
  const el = inviteForm()?.querySelector("select");
  if (!el) throw new Error("no invitation role select on the page");
  return el as HTMLSelectElement;
}

/** The reason a control announces, through `aria-describedby` — the channel a
 *  keyboard or screen-reader operator has, which a `title` is not. */
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
  listTeamMembers.mockReset().mockResolvedValue([]);
  listInvitations.mockReset().mockResolvedValue([]);
  listOrgMembers.mockReset().mockResolvedValue([]);
  getTeam.mockReset();
});
afterEach(cleanup);

describe("TeamPage · the roster's rank gate", () => {
  it("leaves the whole team roster to an admin of the parent org", async () => {
    // Their authority over the team comes from the ORG row, so a grant above
    // their own does not bind them — the org ladder is the whole rule.
    identity.orgs = tree("admin", "viewer");
    listOrgMembers.mockResolvedValue([orgMember("me", "admin"), orgMember("boss", "member")]);
    listTeamMembers.mockResolvedValue([
      teamMember("me", "viewer"),
      teamMember("boss", "owner"),
    ]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    // Enabled once the org roster has landed — until then the standing the
    // org ladder reads is unknown, and an unknown refuses.
    await waitFor(() => expect(select.disabled).toBe(false));
    expect(removeButton("boss").disabled).toBe(false);
    // And no cap on what they write there.
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
      "owner",
    ]);
  });

  it("binds the team ladder when the viewer's org standing is below admin", async () => {
    identity.orgs = tree("member", "admin");
    listOrgMembers.mockResolvedValue([
      orgMember("me", "member"),
      orgMember("boss", "member"),
      orgMember("cfg", "member"),
    ]);
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("boss", "owner"),
      teamMember("cfg", "config_editor"),
    ]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
    expect(removeButton("boss").disabled).toBe(true);
    const reason = announcedReason(select);
    expect(reason).toContain("a member of this team who already holds owner");
    // The remedy a TEAM route accepts: canManageTeam refuses a plain org
    // member, so the org-scope wording would send the operator nowhere.
    expect(reason).toContain("an admin or owner of the team's organization");
    expect(announcedReason(removeButton("boss"))).toBe(reason);
    expect(select.value).toBe("owner");

    // A grant off the ladder outranks nothing, and the row shows what it is.
    expect(roleSelect("cfg").disabled).toBe(false);
    expect(roleSelect("cfg").value).toBe("config_editor");
    // Their own row is theirs: equal rank passes.
    expect(roleSelect("me").disabled).toBe(false);
  });

  it("binds the ORG ladder on a team write, with the remedy that route accepts", async () => {
    // A team admin whose org standing is `member` does not write over the
    // grant of an org admin — C.1, the same comparison the org page makes,
    // with a different remedy because the route is gated differently.
    identity.orgs = tree("member", "admin");
    listOrgMembers.mockResolvedValue([
      orgMember("me", "member"),
      orgMember("orgboss", "admin"),
    ]);
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("orgboss", "member"),
    ]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for orgboss@example.org",
    )) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
    const reason = announcedReason(select);
    expect(reason).toContain("an admin or owner of that organization");
    expect(reason).not.toContain("a member of this org who already holds");
  });

  it("offers no team role above the viewer's own grant, and says so", async () => {
    identity.orgs = tree("member", "admin");
    listOrgMembers.mockResolvedValue([orgMember("me", "member"), orgMember("peer", "member")]);
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("peer", "member"),
    ]);
    mount();

    await screen.findByLabelText("Role for peer@example.org");
    // `owner` is the write the team cap refuses a team admin — on any row,
    // their own included. config_editor is off the ladder and never capped.
    expect(Array.from(roleSelect("peer").options).map((o) => o.value)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
    ]);
    expect(Array.from(roleSelect("me").options).map((o) => o.value)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
    ]);
    expect(screen.getByText(/Roles above your own are not offered \(owner\)/)).toBeTruthy();
  });

  it("takes the viewer's grant from the roster row, not the identity tree", async () => {
    // The tree says admin, from sign-in. The roster says owner — the row the
    // server's guard reads — so the owner grant beside them is theirs.
    identity.orgs = tree("member", "admin");
    listOrgMembers.mockResolvedValue([orgMember("me", "member"), orgMember("boss", "member")]);
    listTeamMembers.mockResolvedValue([
      teamMember("me", "owner"),
      teamMember("boss", "owner"),
    ]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    await waitFor(() => expect(select.disabled).toBe(false));
    expect(removeButton("boss").disabled).toBe(false);
  });

  it("leaves every control enabled for a platform admin who holds no grant", async () => {
    identity.user = { ...identity.user, is_super_admin: true };
    getTeam.mockResolvedValue({
      id: TEAM_ID,
      name: "PIC",
      slug: "pic",
      org_id: ORG_ID,
      personal: false,
    });
    listOrgMembers.mockResolvedValue([orgMember("boss", "owner")]);
    listTeamMembers.mockResolvedValue([teamMember("boss", "owner")]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    await waitFor(() => expect(select.disabled).toBe(false));
    expect(removeButton("boss").disabled).toBe(false);
    expect(select.getAttribute("aria-describedby")).toBeNull();
  });
});

// The delete control reads the TEAM roster too: its rows are the ones the
// deletion clears, so an unread team roster refuses the control before any
// rank is compared.
describe("TeamPage · the delete control waits for the team roster it reads", () => {
  async function deleteButton(): Promise<HTMLButtonElement> {
    return (await screen.findByRole("button", {
      name: "Delete team",
    })) as HTMLButtonElement;
  }
  function primeTeam() {
    getTeam.mockResolvedValue({
      id: TEAM_ID,
      name: "PIC",
      slug: "pic",
      org_id: ORG_ID,
      personal: false,
    });
    // An org admin is the one viewer whose gate arms without the team roster
    // (their org row carries it), so they are the viewer the wait is staged
    // for: a grant-authority viewer renders no delete control at all until
    // the same roster names their grant.
    listOrgMembers.mockResolvedValue([orgMember("me", "admin"), orgMember("peer", "member")]);
    identity.orgs = tree("admin", "member");
  }

  it("offers no delete while the team roster is in flight, and says why", async () => {
    primeTeam();
    let release: (v: TeamMemberView[]) => void = () => {};
    listTeamMembers.mockImplementation(
      () => new Promise<TeamMemberView[]>((res) => (release = res)),
    );
    mount();
    const del = await deleteButton();
    expect(del.disabled).toBe(true);
    expect(announcedReason(del)).toContain("has not finished loading");
    release([teamMember("me", "admin"), teamMember("peer", "member")]);
    await waitFor(() => expect(del.disabled).toBe(false));
  });

  it("offers no delete when the team roster cannot be read, and says why", async () => {
    primeTeam();
    listTeamMembers.mockRejectedValue(new Error("team roster: 500 upstream"));
    mount();
    const del = await deleteButton();
    await waitFor(() => expect(announcedReason(del)).toContain("could not be read"));
    expect(del.disabled).toBe(true);
  });
});

// The org roster is an INPUT of the org ladder, and of the gate that renders
// the controls at all. The states it has — in flight, failed, not there —
// are the states the controls owe an answer in.
describe("TeamPage · the org roster the decision reads", () => {
  it("offers nothing on a row while the org roster is in flight, and says why", async () => {
    // Their authority over the team is their own grant (org member), so the
    // org ladder binds — and the row's org role is the roster's to tell.
    identity.orgs = tree("member", "owner");
    listTeamMembers.mockResolvedValue([
      teamMember("me", "owner"),
      teamMember("boss", "admin"),
    ]);
    let release: (v: OrgMemberView[]) => void = () => {};
    listOrgMembers.mockImplementation(
      () => new Promise<OrgMemberView[]>((res) => (release = res)),
    );
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
    expect(removeButton("boss").disabled).toBe(true);
    expect(announcedReason(select)).toContain("still loading");
    // And the role list is not a ladder the standing has not been read on.
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["admin"]);

    // The server refuses this very cell once the roster is there:
    //   team / org:member+team:owner / org:admin+team:admin → refuse:org-ladder
    release([orgMember("me", "member"), orgMember("boss", "admin")]);
    await waitFor(() =>
      expect(announcedReason(roleSelect("boss"))).toContain(
        "an admin or owner of that organization",
      ),
    );
    expect(roleSelect("boss").disabled).toBe(true);
  });

  it("offers nothing, and reports the failure, when the org roster cannot be read", async () => {
    identity.orgs = tree("member", "owner");
    listTeamMembers.mockResolvedValue([
      teamMember("me", "owner"),
      teamMember("boss", "admin"),
    ]);
    listOrgMembers.mockRejectedValue(new Error("org roster: 500 upstream"));
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    await waitFor(() => expect(announcedReason(select)).toContain("could not be read"));
    expect(select.disabled).toBe(true);
    expect(removeButton("boss").disabled).toBe(true);
    // The reason names a remedy and no org role: none was read (I).
    expect(announcedReason(select)).toContain("platform admin");
    expect(announcedReason(select)).not.toContain("is admin");
    // And the failure is reported where this page reports its load failures,
    // not only in the row it disables.
    expect(screen.getByText(/org roster: 500 upstream/)).toBeTruthy();
  });

  it("stops deciding from org rows a failed refetch could not refresh", async () => {
    // react-query keeps `data` through a failed refetch and sets `error`: the
    // one state where the order of the two decides anything, and the one this
    // page reaches on every reload() after an org-roster outage. The rows are
    // stale and the page holds no current read of them, so every control the
    // org ladder decides is refused with THAT reason — a row left enabled
    // from stale rows is enabled for good.
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("peer", "member"),
    ]);
    listOrgMembers
      .mockResolvedValueOnce([orgMember("me", "member"), orgMember("peer", "member")])
      .mockRejectedValue(new Error("org roster: 503 refetch"));
    const { qc } = mount();

    // The rows landed: a peer at an org rank the viewer covers is theirs.
    await waitFor(() => expect(roleSelect("peer").disabled).toBe(false));

    await refetchOrgRoster(qc);

    await waitFor(() => expect(roleSelect("peer").disabled).toBe(true));
    expect(announcedReason(roleSelect("peer"))).toContain("could not be read");
    expect(removeButton("peer").disabled).toBe(true);
    // The invitation form the same standing decides goes with it.
    expect(inviteRoleSelect().disabled).toBe(true);
    expect(announcedReason(inviteRoleSelect())).toContain("could not be read");
    expect(screen.getByText(/org roster: 503 refetch/)).toBeTruthy();
  });

  it("withholds every candidate when the roster failed, and says why it did", async () => {
    // Same state, the other surface it decides: the pool is built from rows
    // the page has declared unreadable, so it offers nobody — and the reason
    // it gives is the one that applies. A rank sentence here would name a
    // comparison that was never made and send the operator to an org admin
    // who cannot help.
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([teamMember("me", "admin")]);
    listOrgMembers
      .mockResolvedValueOnce([
        orgMember("me", "member"),
        orgMember("cand", "member"),
      ])
      .mockRejectedValue(new Error("org roster: 503 refetch"));
    const { qc } = mount();

    // The candidate the stale rows allow is offered while they are current.
    fireEvent.click(await screen.findByLabelText("Account"));
    expect(await screen.findByText("cand@example.org")).toBeTruthy();

    await refetchOrgRoster(qc);

    await waitFor(() =>
      expect(
        screen.getByText(
          /This organization's roster could not be read, so no candidate is offered/,
        ),
      ).toBeTruthy(),
    );
    expect(screen.queryAllByText("cand@example.org")).toHaveLength(0);
    // Not "their role in this organization is above your own standing".
    expect(screen.queryByText(/is above your own\s+standing there/)).toBeNull();
    expect(screen.queryByText(/of them is not\s+offered/)).toBeNull();
    expect(screen.queryByText(/of them are not\s+offered/)).toBeNull();
  });

  it("refuses the invitation form the same standing decides, with that reason", async () => {
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([teamMember("me", "admin")]);
    listOrgMembers.mockRejectedValue(new Error("org roster: 503"));
    mount();

    const send = (await screen.findByRole("button", {
      name: "Send invite",
    })) as HTMLButtonElement;
    await waitFor(() => expect(send.disabled).toBe(true));
    expect(inviteRoleSelect().disabled).toBe(true);
    expect(announcedReason(send)).toContain("could not be read");
  });
});

// The gate: who sees a control at all. Read from the identity tree it kept
// both failures the ladder moved off it.
describe("TeamPage · the gate the roster decides", () => {
  it("renders the controls a promotion the roster already shows", async () => {
    // The tree says viewer — fetched at sign-in, and no roster write
    // refreshes it. The roster says owner, which is the row canManageTeam
    // reads.
    identity.orgs = tree("member", "viewer");
    listTeamMembers.mockResolvedValue([
      teamMember("me", "owner"),
      teamMember("peer", "member"),
    ]);
    listOrgMembers.mockResolvedValue([orgMember("me", "member"), orgMember("peer", "member")]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for peer@example.org",
    )) as HTMLSelectElement;
    await waitFor(() => expect(select.disabled).toBe(false));
    expect(removeButton("peer").disabled).toBe(false);
  });

  it("renders no control after a self-demotion the roster already shows", async () => {
    // The tree still says admin; the roster says viewer, and the server
    // answers canManageTeam's 403 on every row.
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([
      teamMember("me", "viewer"),
      teamMember("peer", "member"),
    ]);
    listOrgMembers.mockResolvedValue([orgMember("me", "member"), orgMember("peer", "member")]);
    mount();

    await screen.findByText("peer@example.org");
    await waitFor(() => expect(listOrgMembers).toHaveBeenCalled());
    expect(screen.queryAllByLabelText("Role for peer@example.org")).toHaveLength(0);
    expect(screen.queryAllByRole("button", { name: "Remove" })).toHaveLength(0);
    expect(screen.queryByText("Invite a member")).toBeNull();
    // The roster is still READ — only writing it is refused.
    expect(screen.getByText("member")).toBeTruthy();
  });

  it("reads no org standing out of a team grant when the org roster has no row for them", async () => {
    // The reachable state: the roster ANSWERED — so it is not the unknown
    // `orgStandingBlock` covers — and carries no row for this viewer, because
    // their org membership has just been removed (`reload()` refetched it).
    // Their standing there is none, which every org row outranks; reading
    // their TEAM grant as an org standing would re-open the whole ladder.
    identity.orgs = tree("member", "owner");
    listTeamMembers.mockResolvedValue([
      teamMember("me", "owner"),
      teamMember("boss", "member"),
    ]);
    listOrgMembers.mockResolvedValue([orgMember("boss", "admin")]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    await waitFor(() => expect(select.disabled).toBe(true));
    expect(announcedReason(select)).toContain(
      "an admin or owner of that organization",
    );
    // And the cap on what they could write anywhere is the team grant alone.
    expect(Array.from(roleSelect("me").options).map((o) => o.value)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
      "owner",
    ]);
  });

  it("keeps the org-admin arm of the gate, from the org roster row", async () => {
    // No grant on the team at all, an org admin row: the server's
    // orgAdminOfTeam arm, read from the roster rather than the tree.
    identity.orgs = tree("member", "viewer");
    listTeamMembers.mockResolvedValue([teamMember("peer", "owner")]);
    listOrgMembers.mockResolvedValue([orgMember("me", "admin"), orgMember("peer", "member")]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for peer@example.org",
    )) as HTMLSelectElement;
    await waitFor(() => expect(select.disabled).toBe(false));
  });
});

// Spec D: a team with no parent org. The fact is the team row's own
// `org_id`, which the non-member path of the subject hook carries.
describe("TeamPage · what the invitation form announces", () => {
  it("announces why its ladder is short, with ids that belong to the form", async () => {
    // A team admin who is a plain org member: the picker is narrowed (the org
    // mirror of admin and owner is org admin, which they do not hold). The
    // note that says so is the explanation a reader focused on the select
    // needs, so the select points at it — a <p> nothing references is text
    // for a mouse.
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([teamMember("me", "admin")]);
    listOrgMembers.mockResolvedValue([orgMember("me", "member")]);
    mount();

    await screen.findByRole("button", { name: "Send invite" });
    const select = inviteRoleSelect();
    await waitFor(() => expect(select.disabled).toBe(false));
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      "config_editor",
      "viewer",
      "member",
    ]);
    expect(announcedReason(select)).toContain(
      "Roles an invitation of yours cannot carry are not offered",
    );

    // A second instance of the same form names its OWN reasons: the ids come
    // from the instance, so two Members sections on one page do not describe
    // each other's controls.
    mount();
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: "Send invite" })).toHaveLength(2),
    );
    const described = screen
      .getAllByRole("button", { name: "Send invite" })
      .map((b) => b.getAttribute("aria-describedby"));
    expect(described[0]).toBeTruthy();
    expect(described[1]).toBeTruthy();
    expect(described[0]).not.toBe(described[1]);
  });
});

describe("TeamPage · a team with no parent org", () => {
  beforeEach(() => {
    // Such a team is in no org, so the identity tree — which enumerates org
    // memberships — never carries it: the page resolves it through getTeam.
    identity.orgs = [];
    getTeam.mockResolvedValue({ id: TEAM_ID, name: "PIC", slug: "pic", personal: false });
  });

  it("decides on the grant alone, and offers no org remedy", async () => {
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("boss", "owner"),
      teamMember("peer", "member"),
    ]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
    const reason = announcedReason(select);
    expect(reason).toContain("a member of this team who already holds owner");
    expect(reason).toContain("This team has no organization to appeal to");
    expect(reason).not.toContain("an admin or owner of the team's organization");
    // A grant at or below their own is theirs, with no org roster read at
    // all — the query that would read one is not asked.
    await waitFor(() => expect(roleSelect("peer").disabled).toBe(false));
    expect(listOrgMembers).not.toHaveBeenCalled();
    // And nothing waits for a roster that does not exist.
    expect(screen.queryByText(/still loading/)).toBeNull();
  });

  it("renders no candidate panel at all, in no state of a roster it never reads", async () => {
    // There is no org roster here, so there is nobody to place from: the
    // panel is not rendered rather than rendered empty. It is the same fact
    // that decides its loading state, which is why the two cannot drift —
    // and why nothing here can sit in "Loading accounts…" forever (a
    // disabled react-query stays `isPending`, so `isPending` is not it), nor
    // offer an org-shaped message to a team that has no org.
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("peer", "member"),
    ]);
    mount();

    await screen.findByText("peer@example.org");
    await waitFor(() => expect(roleSelect("peer").disabled).toBe(false));
    expect(candidatePanel()).toBeNull();
    expect(screen.queryByPlaceholderText("Loading accounts…")).toBeNull();
    expect(
      screen.queryByText(/Every member of this organization is already on the team/),
    ).toBeNull();
    expect(listOrgMembers).not.toHaveBeenCalled();
  });

  it("reads an empty org id as no parent org, not as an org to ask about", async () => {
    // `org_id` is `omitempty` server-side, so a team with no parent leaves it
    // out — and the generated type still allows the empty string. Asking for
    // the roster of org "" would answer a failure, and the page would then
    // refuse every row for an organization that does not exist instead of
    // taking the grant's own ladder (D).
    getTeam.mockResolvedValue({
      id: TEAM_ID,
      name: "PIC",
      slug: "pic",
      personal: false,
      org_id: "",
    });
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("boss", "owner"),
      teamMember("peer", "member"),
    ]);
    mount();

    const select = (await screen.findByLabelText(
      "Role for boss@example.org",
    )) as HTMLSelectElement;
    await waitFor(() => expect(select.disabled).toBe(true));
    expect(announcedReason(select)).toContain(
      "This team has no organization to appeal to",
    );
    expect(announcedReason(select)).not.toContain("could not be read");
    expect(roleSelect("peer").disabled).toBe(false);
    expect(listOrgMembers).not.toHaveBeenCalled();
    expect(candidatePanel()).toBeNull();
  });

  it("caps the invitation at the minter's own grant, with no mirror to read", async () => {
    listTeamMembers.mockResolvedValue([teamMember("me", "admin")]);
    mount();

    await screen.findByRole("button", { name: "Send invite" });
    const select = inviteRoleSelect();
    await waitFor(() => expect(select.disabled).toBe(false));
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
    ]);
  });
});

describe("TeamPage · the surfaces that place and invite", () => {
  it("withholds a candidate the org ladder puts above the viewer", async () => {
    // The PUT this pool feeds takes the same guard as the rows:
    //   team / org:member+team:owner / org:admin+team:none → refuse:org-ladder
    identity.orgs = tree("member", "owner");
    listTeamMembers.mockResolvedValue([teamMember("me", "owner")]);
    listOrgMembers.mockResolvedValue([
      orgMember("me", "member"),
      orgMember("boss", "admin"),
      orgMember("peer", "member"),
    ]);
    mount();

    fireEvent.click(await screen.findByLabelText("Account"));
    expect(await screen.findByText("peer@example.org")).toBeTruthy();
    await waitFor(() =>
      expect(screen.queryAllByText("boss@example.org")).toHaveLength(0),
    );
    // And the operator is told, rather than left wondering who is missing.
    expect(screen.getByText(/1 of them is not offered/)).toBeTruthy();
  });

  it("offers only the roles an invitation of this minter may carry", async () => {
    // A team admin who is a plain org member: the org MIRROR of admin and
    // owner is org admin, which they do not hold, so
    // requireOrgStandingToInvite refuses both.
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([teamMember("me", "admin")]);
    listOrgMembers.mockResolvedValue([orgMember("me", "member")]);
    mount();

    await screen.findByRole("button", { name: "Send invite" });
    const select = inviteRoleSelect();
    await waitFor(() => expect(select.disabled).toBe(false));
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      "config_editor",
      "viewer",
      "member",
    ]);
    expect(
      screen.getByText(/Roles an invitation of yours cannot carry are not offered/),
    ).toBeTruthy();
  });

  it("sends the role the select shows when the offered list shrinks under a pick", async () => {
    // The org roster lands after the operator has picked: the pick is gone
    // from the list, and what is submitted is what they can still see.
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([teamMember("me", "admin")]);
    listOrgMembers.mockResolvedValue([orgMember("me", "member")]);
    mount();

    await screen.findByRole("button", { name: "Send invite" });
    const select = inviteRoleSelect();
    await waitFor(() => expect(select.disabled).toBe(false));
    fireEvent.change(select, { target: { value: "member" } });
    expect(select.value).toBe("member");
    // `owner` is not on offer, so a held pick of it reconciles DOWN to the
    // highest role that is — and the select shows that, not config_editor.
    expect(Array.from(select.options).map((o) => o.value)).not.toContain("owner");
  });
});

// A grant the viewer holds on a team whose ORG roster the server refuses them.
// `GET /api/orgs/{id}/members` is gated on canViewOrg, which admits every
// member of the org, so a 403 there states that they hold no membership in the
// team's organization: their grant is the `orphan_grant` drift the admin
// console exists to surface, and the one `useCanManageTeam` already answers
// false on. The server's own canManageTeam takes a roster write on that grant
// alone — this tab offers none of them, and says why.
describe("TeamPage · a grant whose org membership is missing", () => {
  /** The refusal the handler really sends, prefix included. */
  const refused = () => apiErrorFrom(403, { message: "not a member of this org" });

  beforeEach(() => {
    // No org membership anywhere: the identity tree carries no org, and the
    // parent org is known from the team row alone.
    identity.orgs = [];
    getTeam.mockResolvedValue({
      id: TEAM_ID,
      name: "PIC",
      slug: "pic",
      personal: false,
      org_id: ORG_ID,
    });
  });

  it("offers no roster write control, and names the membership that is missing", async () => {
    listOrgMembers.mockRejectedValue(refused());
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      // Two rows the SERVER would let this author write — its team ladder caps
      // on the grant, and no org row exists to compare.
      teamMember("cfg", "config_editor"),
      teamMember("peer", "member"),
    ]);
    mount();

    await screen.findByText("peer@example.org");
    await waitFor(() =>
      expect(
        screen.getByText(/You hold no membership in this team's organization/),
      ).toBeTruthy(),
    );
    // The roster is read-only: the role is text, with no select and no Remove
    // beside it, and neither write surface is rendered at all.
    expect(screen.queryByLabelText("Role for peer@example.org")).toBeNull();
    expect(screen.queryByLabelText("Role for cfg@example.org")).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Send invite" })).toBeNull();
    expect(candidatePanel()).toBeNull();
  });

  it("names the remedy that repairs it, and not the reload that cannot", async () => {
    listOrgMembers.mockRejectedValue(refused());
    listTeamMembers.mockResolvedValue([teamMember("me", "owner")]);
    mount();

    const note = await screen.findByText(
      /You hold no membership in this team's organization/,
    );
    expect(note.textContent).toContain("an admin or owner of that organization");
    // A reload re-issues the same 403 for good: what repairs this is the org
    // membership, not another read.
    expect(note.textContent).not.toContain("reload");
    // And the transport's own line is not shown beside it: the server
    // answered, and what it answered is stated in full.
    expect(screen.queryByText(/API error 403/)).toBeNull();
  });

  it("keeps the surface, and the reason that names a reload, when the read FAILS", async () => {
    // The sibling state, unchanged: an outage is an unknown, the gate still
    // rests on the grant, and every control the org ladder decides is refused
    // in the row with the reason an outage deserves.
    listOrgMembers.mockRejectedValue(new Error("org roster: 503 upstream"));
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("peer", "member"),
    ]);
    mount();

    await screen.findByRole("button", { name: "Send invite" });
    await waitFor(() => expect(roleSelect("peer").disabled).toBe(true));
    expect(announcedReason(roleSelect("peer"))).toContain("could not be read");
    expect(announcedReason(roleSelect("peer"))).toContain("reload the page");
    expect(
      screen.queryByText(/You hold no membership in this team's organization/),
    ).toBeNull();
    expect(screen.getByText(/org roster: 503 upstream/)).toBeTruthy();
  });
});

describe("TeamPage · two Members sections in one document", () => {
  it("carries no duplicate id", async () => {
    // Every id this section renders is unique to its instance, the row
    // reasons included: `getElementById` resolves the FIRST, so a duplicated
    // reason id makes the second section's controls describe the first's text.
    identity.orgs = tree("member", "admin");
    listTeamMembers.mockResolvedValue([
      teamMember("me", "admin"),
      teamMember("boss", "owner"),
    ]);
    listOrgMembers.mockResolvedValue([
      orgMember("me", "member"),
      orgMember("boss", "member"),
    ]);
    mount();
    await screen.findByText("boss@example.org");
    mount();
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: "Send invite" })).toHaveLength(2),
    );

    const all = Array.from(globalThis.document.querySelectorAll("[id]")).map((e) => e.id);
    expect(all.filter((id, i) => all.indexOf(id) !== i)).toEqual([]);
    // Not vacuous: the refused row's reason is rendered by both sections, and
    // the two ids differ.
    const rowReasons = all.filter((id) => id.endsWith("-team-member-rank-boss"));
    expect(rowReasons).toHaveLength(2);
    expect(new Set(rowReasons).size).toBe(2);
  });
});
