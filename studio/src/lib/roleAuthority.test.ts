import { describe, expect, it } from "vitest";

import {
  assignableRoles,
  assignableRolesNote,
  canDeleteTeam,
  canManageFromRoster,
  canWriteMember,
  invitableRoles,
  invitableRolesNote,
  orgMirrorOfTeamRole,
  orgStandingBlock,
  reconcileRole,
  roleOptions,
  rosterReadOf,
  teamScopeFor,
  type MemberRow,
  type RosterRead,
  type ViewerStanding,
  type WriteScope,
} from "./roleAuthority";
import { ORG_ROLES, TEAM_ROLES } from "./roles";
// The error every non-2xx response throws: the classifier reads its status, so
// the probe hands it the shape production produces.
import { apiErrorFrom } from "@/api/client";

// The oracle is the SERVER's shape, not this module's: ranks and `AtLeast`
// (pkg/identity/types.go), then requireRankOverMember's own order of
// questions (pkg/server/auth_authz.go) and the two caps on the role written.
// Written from ranks rather than from ladder indexes on purpose — a test that
// re-derived the implementation's `indexOf` arithmetic would pass whatever
// that arithmetic says, which is the one thing worth checking.
const ORG_RANK: Record<string, number> = { member: 1, admin: 2, owner: 3 };
const TEAM_RANK: Record<string, number> = { viewer: 1, member: 2, admin: 3, owner: 4 };

function atLeast(
  have: string | null,
  want: string | null,
  rank: Record<string, number>,
): boolean {
  const h = have ? (rank[have] ?? 0) : 0;
  const w = want ? (rank[want] ?? 0) : 0;
  if (h === 0 || w === 0) return false;
  return h >= w;
}

/** requireRankOverMember, in the server's own order. */
function serverAllowsWrite(
  viewer: ViewerStanding,
  row: MemberRow,
  scope: WriteScope,
): boolean {
  if (viewer.platformAdmin) return true;
  const orgKnown = scope.kind === "org" || scope.hasParentOrg;
  const standing = orgKnown ? viewer.orgRole : null;
  if (orgKnown && row.orgRole != null && !atLeast(standing, row.orgRole, ORG_RANK)) {
    return false;
  }
  if (scope.kind === "org" || atLeast(standing, "admin", ORG_RANK)) return true;
  if (row.teamRole == null || row.teamRole === "config_editor") return true;
  return atLeast(viewer.teamRole, row.teamRole, TEAM_RANK);
}

/** The two caps on the role WRITTEN: the org standing, and the team grant. */
function serverAssignable(viewer: ViewerStanding, scope: WriteScope): string[] {
  const ladder: string[] = scope.kind === "org" ? [...ORG_ROLES] : [...TEAM_ROLES];
  if (viewer.platformAdmin) return ladder;
  if (scope.kind === "org") {
    return ladder.filter((r) => atLeast(viewer.orgRole, r, ORG_RANK));
  }
  const standing = scope.hasParentOrg ? viewer.orgRole : null;
  if (atLeast(standing, "admin", ORG_RANK)) return ladder;
  // config_editor is off the ladder and never capped (ADR-078).
  return ladder.filter(
    (r) => r === "config_editor" || atLeast(viewer.teamRole, r, TEAM_RANK),
  );
}

// Every value either ladder can carry, plus one this build cannot place and
// the absence of a row — the two the server ranks 0 and refuses.
const ORG_VALUES = [null, "member", "admin", "owner", "config_editor", "archon"] as const;
const TEAM_VALUES = [
  null,
  "viewer",
  "member",
  "admin",
  "owner",
  "config_editor",
  "archon",
] as const;

/** Whether a ladder carries a value at all — the widened test of a readonly
 *  role tuple, which `includes` refuses on its own element type. */
function onLadder(role: string, ladder: readonly string[]): boolean {
  return ladder.includes(role);
}

function viewer(over: Partial<ViewerStanding> = {}): ViewerStanding {
  return { platformAdmin: false, orgRole: null, teamRole: null, ...over };
}

const ORG: WriteScope = { kind: "org" };
const TEAM: WriteScope = { kind: "team", hasParentOrg: true };
const ORPHAN_TEAM: WriteScope = { kind: "team", hasParentOrg: false };

// Every (viewer, row) the org roster can present: 6 standings × 5 rows (the
// roster always carries a role, so `null` is not a row there) × exempt or not.
function orgCases(): Array<{ viewer: ViewerStanding; row: MemberRow }> {
  const out: Array<{ viewer: ViewerStanding; row: MemberRow }> = [];
  for (const platformAdmin of [false, true]) {
    for (const mine of ORG_VALUES) {
      for (const theirs of ORG_VALUES.filter((v) => v !== null)) {
        out.push({
          viewer: viewer({ platformAdmin, orgRole: mine }),
          row: { orgRole: theirs, teamRole: null },
        });
      }
    }
  }
  return out;
}

// Every (viewer, row) a team roster can present, both ladders at once.
function teamCases(): Array<{ viewer: ViewerStanding; row: MemberRow }> {
  const out: Array<{ viewer: ViewerStanding; row: MemberRow }> = [];
  for (const myOrg of ORG_VALUES) {
    for (const myTeam of TEAM_VALUES) {
      for (const theirOrg of ORG_VALUES) {
        for (const theirTeam of TEAM_VALUES.filter((v) => v !== null)) {
          out.push({
            viewer: viewer({ orgRole: myOrg, teamRole: myTeam }),
            row: { orgRole: theirOrg, teamRole: theirTeam },
          });
        }
      }
    }
  }
  return out;
}

describe("canWriteMember — exhaustive, against the server's rule", () => {
  it("answers the org roster the way requireRankOverMember does", () => {
    const wrong = orgCases().filter(
      ({ viewer: v, row }) =>
        canWriteMember(v, row, ORG).allowed !== serverAllowsWrite(v, row, ORG),
    );
    expect(wrong.map((c) => JSON.stringify(c))).toEqual([]);
    // The cross product is the claim: a shrunk one proves less than it reads.
    expect(orgCases()).toHaveLength(2 * 6 * 5);
  });

  it("answers a team roster the way requireRankOverMember does, both ladders", () => {
    const wrong = teamCases().filter(
      ({ viewer: v, row }) =>
        canWriteMember(v, row, TEAM).allowed !== serverAllowsWrite(v, row, TEAM),
    );
    expect(wrong.map((c) => JSON.stringify(c))).toEqual([]);
    expect(teamCases()).toHaveLength(6 * 7 * 6 * 6);
  });

  it("drops the org ladder on a team with no parent org, and keeps the grant's", () => {
    const wrong = teamCases().filter(
      ({ viewer: v, row }) =>
        canWriteMember(v, row, ORPHAN_TEAM).allowed !==
        serverAllowsWrite(v, row, ORPHAN_TEAM),
    );
    expect(wrong.map((c) => JSON.stringify(c))).toEqual([]);
  });

  it("exempts a platform admin from both ladders", () => {
    const admin = viewer({ platformAdmin: true });
    for (const scope of [ORG, TEAM, ORPHAN_TEAM]) {
      expect(
        canWriteMember(admin, { orgRole: "owner", teamRole: "owner" }, scope),
      ).toEqual({ allowed: true, reason: null });
    }
  });
});

// The cases the spec names, as literals: an oracle shared by every row of the
// cross product above cannot catch its own mistake, and these are the rows an
// operator meets.
describe("canWriteMember — the spec's named cases", () => {
  it("refuses an org row above the viewer's standing, and passes equal rank", () => {
    const admin = viewer({ orgRole: "admin" });
    expect(canWriteMember(admin, { orgRole: "owner", teamRole: null }, ORG).allowed).toBe(
      false,
    );
    expect(canWriteMember(admin, { orgRole: "admin", teamRole: null }, ORG).allowed).toBe(
      true,
    );
    expect(canWriteMember(admin, { orgRole: "member", teamRole: null }, ORG).allowed).toBe(
      true,
    );
  });

  it("is outranked by every rung when the viewer holds no org row", () => {
    const none = viewer();
    for (const role of ["member", "admin", "owner"]) {
      expect(canWriteMember(none, { orgRole: role, teamRole: null }, ORG).allowed).toBe(
        false,
      );
    }
  });

  it("leaves an org admin of the parent org the org ladder alone on a team", () => {
    // Their authority over the team is the ORG row, so a grant above their
    // own does not bind them — canManageTeam admits them through the org.
    const orgAdmin = viewer({ orgRole: "admin", teamRole: "viewer" });
    expect(
      canWriteMember(orgAdmin, { orgRole: "member", teamRole: "owner" }, TEAM).allowed,
    ).toBe(true);
    // The org ladder still does: an org owner outranks them there.
    expect(
      canWriteMember(orgAdmin, { orgRole: "owner", teamRole: "viewer" }, TEAM).allowed,
    ).toBe(false);
  });

  it("binds the team ladder when the viewer's org standing is below admin", () => {
    const teamAdmin = viewer({ orgRole: "member", teamRole: "admin" });
    expect(
      canWriteMember(teamAdmin, { orgRole: "member", teamRole: "owner" }, TEAM).allowed,
    ).toBe(false);
    expect(
      canWriteMember(teamAdmin, { orgRole: "member", teamRole: "admin" }, TEAM).allowed,
    ).toBe(true);
    // A grant off the ladder, and no grant at all, rank below every rung.
    expect(
      canWriteMember(teamAdmin, { orgRole: "member", teamRole: "config_editor" }, TEAM)
        .allowed,
    ).toBe(true);
    expect(
      canWriteMember(teamAdmin, { orgRole: "member", teamRole: null }, TEAM).allowed,
    ).toBe(true);
  });

  it("writes the viewer's own row, on either ladder", () => {
    const own = viewer({ orgRole: "admin", teamRole: "admin" });
    expect(canWriteMember(own, { orgRole: "admin", teamRole: null }, ORG).allowed).toBe(
      true,
    );
    const teamOwn = viewer({ orgRole: "member", teamRole: "admin" });
    expect(
      canWriteMember(teamOwn, { orgRole: "member", teamRole: "admin" }, TEAM).allowed,
    ).toBe(true);
  });

  it("keeps the grant's ladder on a team with no parent org, whatever is held elsewhere", () => {
    // D: no org ladder — the standing the viewer holds in some OTHER org
    // must not promote them here, and the grant decides alone.
    const elsewhere = viewer({ orgRole: "owner", teamRole: "member" });
    expect(
      canWriteMember(elsewhere, { orgRole: "owner", teamRole: "admin" }, ORPHAN_TEAM)
        .allowed,
    ).toBe(false);
    expect(
      canWriteMember(elsewhere, { orgRole: "owner", teamRole: "member" }, ORPHAN_TEAM)
        .allowed,
    ).toBe(true);
  });

  it("refuses a rank this build cannot place, on either ladder", () => {
    const owner = viewer({ orgRole: "owner", teamRole: "owner" });
    expect(canWriteMember(owner, { orgRole: "archon", teamRole: null }, ORG).allowed).toBe(
      false,
    );
    const teamOwner = viewer({ orgRole: "member", teamRole: "owner" });
    expect(
      canWriteMember(teamOwner, { orgRole: "member", teamRole: "archon" }, TEAM).allowed,
    ).toBe(false);
    // config_editor is exactly that on the ORG ladder, where no rung carries
    // it: the server ranks it 0 there and refuses. The reason renders it the
    // way every other surface does, not as the stored string.
    const offLadderOnOrg = canWriteMember(
      owner,
      { orgRole: "config_editor", teamRole: null },
      ORG,
    );
    expect(offLadderOnOrg.allowed).toBe(false);
    expect(offLadderOnOrg.reason).toContain("Config editor");
  });

  it("names a remedy that works, for the route the write takes", () => {
    const orgReason = canWriteMember(
      viewer({ orgRole: "admin" }),
      { orgRole: "owner", teamRole: null },
      ORG,
    ).reason;
    expect(orgReason).toContain("a member of this org who already holds owner");
    expect(orgReason).toContain("platform admin");

    // On a TEAM route the org-ladder remedy is an admin or owner of the org:
    // canManageTeam refuses the plain org member the org wording would send.
    const teamOrgReason = canWriteMember(
      viewer({ orgRole: "member", teamRole: "admin" }),
      { orgRole: "admin", teamRole: "member" },
      TEAM,
    ).reason;
    expect(teamOrgReason).toContain("an admin or owner of that organization");
    expect(teamOrgReason).not.toContain("a member of this org who already holds");

    const teamReason = canWriteMember(
      viewer({ orgRole: "member", teamRole: "admin" }),
      { orgRole: "member", teamRole: "owner" },
      TEAM,
    ).reason;
    expect(teamReason).toContain("a member of this team who already holds owner");
    expect(teamReason).toContain("an admin or owner of the team's organization");

    // A team with no org to appeal to offers no org remedy.
    const orphanReason = canWriteMember(
      viewer({ teamRole: "admin" }),
      { orgRole: null, teamRole: "owner" },
      ORPHAN_TEAM,
    ).reason;
    expect(orphanReason).toContain("a member of this team who already holds owner");
    expect(orphanReason).not.toContain("an admin or owner of");

    // A rank nothing can place names the one remedy that works.
    const unplaceable = canWriteMember(
      viewer({ orgRole: "owner" }),
      { orgRole: "archon", teamRole: null },
      ORG,
    ).reason;
    expect(unplaceable).toContain("a platform admin writes this row");
    expect(unplaceable).not.toContain("already holds");
  });

  it("carries a reason exactly when it refuses", () => {
    for (const scope of [ORG, TEAM, ORPHAN_TEAM]) {
      const cases = scope.kind === "org" ? orgCases() : teamCases();
      for (const { viewer: v, row } of cases) {
        const d = canWriteMember(v, row, scope);
        if (d.allowed) {
          expect(d.reason).toBeNull();
        } else {
          expect(d.reason).toBeTruthy();
          // Every refusal names the remedy that always works.
          expect(d.reason).toContain("platform admin");
        }
      }
    }
  });
});

describe("assignableRoles", () => {
  it("caps the org ladder at the viewer's own standing", () => {
    expect(assignableRoles(viewer({ orgRole: "owner" }), ORG)).toEqual([
      "member",
      "admin",
      "owner",
    ]);
    expect(assignableRoles(viewer({ orgRole: "admin" }), ORG)).toEqual(["member", "admin"]);
    expect(assignableRoles(viewer({ orgRole: "member" }), ORG)).toEqual(["member"]);
    expect(assignableRoles(viewer(), ORG)).toEqual([]);
    expect(assignableRoles(viewer({ platformAdmin: true }), ORG)).toEqual([
      "member",
      "admin",
      "owner",
    ]);
  });

  it("caps the team ladder at the viewer's own grant, config_editor excepted", () => {
    expect(assignableRoles(viewer({ orgRole: "member", teamRole: "owner" }), TEAM)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
      "owner",
    ]);
    expect(assignableRoles(viewer({ orgRole: "member", teamRole: "admin" }), TEAM)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
    ]);
    expect(assignableRoles(viewer({ orgRole: "member", teamRole: "viewer" }), TEAM)).toEqual([
      "config_editor",
      "viewer",
    ]);
    // A grant off the ladder covers nothing on it, and no grant at all covers
    // nothing either — both keep the one role that is never capped.
    expect(
      assignableRoles(viewer({ orgRole: "member", teamRole: "config_editor" }), TEAM),
    ).toEqual(["config_editor"]);
    expect(assignableRoles(viewer({ orgRole: "member" }), TEAM)).toEqual(["config_editor"]);
  });

  it("leaves the whole team ladder to an admin or owner of the parent org", () => {
    expect(assignableRoles(viewer({ orgRole: "admin", teamRole: "viewer" }), TEAM)).toEqual([
      ...TEAM_ROLES,
    ]);
    expect(assignableRoles(viewer({ orgRole: "owner" }), TEAM)).toEqual([...TEAM_ROLES]);
    expect(assignableRoles(viewer({ platformAdmin: true }), TEAM)).toEqual([...TEAM_ROLES]);
  });

  it("ignores an org standing a team with no parent org does not have", () => {
    expect(
      assignableRoles(viewer({ orgRole: "owner", teamRole: "member" }), ORPHAN_TEAM),
    ).toEqual(["config_editor", "viewer", "member"]);
  });

  it("offers what the server's caps accept, over the whole cross product", () => {
    for (const scope of [ORG, TEAM, ORPHAN_TEAM]) {
      for (const platformAdmin of [false, true]) {
        for (const orgRole of ORG_VALUES) {
          for (const teamRole of TEAM_VALUES) {
            const v = viewer({ platformAdmin, orgRole, teamRole });
            expect({ v, scope, got: [...assignableRoles(v, scope)] }).toEqual({
              v,
              scope,
              got: serverAssignable(v, scope),
            });
          }
        }
      }
    }
  });

  it("offers the role of every ladder row it lets a bound viewer write", () => {
    // What makes a filtered <select> safe on a writable row: its stored value
    // is one of the options, so the control never shows a role it is not.
    // True of the LADDERS, which decide both answers by the same comparison.
    // Two rows fall outside that and need roleOptions() below: one a platform
    // admin reaches (they are exempt from the comparison), and one holding a
    // value no rung carries (nothing can be offered for it).
    for (const { viewer: v, row } of orgCases()) {
      if (v.platformAdmin || row.orgRole == null || !onLadder(row.orgRole, ORG_ROLES)) {
        continue;
      }
      if (canWriteMember(v, row, ORG).allowed) {
        expect(assignableRoles(v, ORG)).toContain(row.orgRole);
      }
    }
    for (const scope of [TEAM, ORPHAN_TEAM]) {
      for (const { viewer: v, row } of teamCases()) {
        if (
          v.platformAdmin ||
          row.teamRole == null ||
          !onLadder(row.teamRole, TEAM_ROLES)
        ) {
          continue;
        }
        if (canWriteMember(v, row, scope).allowed) {
          expect(assignableRoles(v, scope)).toContain(row.teamRole);
        }
      }
    }
  });
});

describe("roleOptions", () => {
  it("leaves an offered role where it is", () => {
    expect(roleOptions(["member", "admin"], "admin")).toEqual(["member", "admin"]);
  });

  it("carries a role the ladder does not offer, so the select shows what is stored", () => {
    // The platform-admin case: an org row left holding an off-ladder value.
    expect(roleOptions(ORG_ROLES, "config_editor")).toEqual([
      "member",
      "admin",
      "owner",
      "config_editor",
    ]);
    expect(roleOptions(ORG_ROLES, "archon")).toContain("archon");
  });

  it("gives every writable row an option for the role it holds", () => {
    for (const { viewer: v, row } of orgCases()) {
      if (!canWriteMember(v, row, ORG).allowed || row.orgRole == null) continue;
      expect(roleOptions(assignableRoles(v, ORG), row.orgRole)).toContain(row.orgRole);
    }
    for (const scope of [TEAM, ORPHAN_TEAM]) {
      for (const { viewer: v, row } of teamCases()) {
        if (!canWriteMember(v, row, scope).allowed || row.teamRole == null) continue;
        expect(roleOptions(assignableRoles(v, scope), row.teamRole)).toContain(
          row.teamRole,
        );
      }
    }
  });
});

describe("assignableRolesNote", () => {
  it("says nothing when the whole ladder is offered", () => {
    expect(assignableRolesNote(viewer({ orgRole: "owner" }), ORG)).toBeNull();
    expect(assignableRolesNote(viewer({ platformAdmin: true }), TEAM)).toBeNull();
    expect(assignableRolesNote(viewer({ orgRole: "admin" }), TEAM)).toBeNull();
  });

  it("names the roles it does not offer, and who writes them", () => {
    const org = assignableRolesNote(viewer({ orgRole: "admin" }), ORG);
    expect(org).toContain("owner");
    expect(org).toContain("that standing in this org");
    const team = assignableRolesNote(viewer({ orgRole: "member", teamRole: "admin" }), TEAM);
    expect(team).toContain("owner");
    expect(team).toContain("an admin or owner of the team's organization");
    const orphan = assignableRolesNote(viewer({ teamRole: "admin" }), ORPHAN_TEAM);
    expect(orphan).toContain("owner");
    expect(orphan).not.toContain("an admin or owner of");
  });

  it("never withholds the role that sits off the ladder", () => {
    // config_editor is never capped, so it is never in the list — the note
    // would otherwise promise a cap the server does not apply.
    for (const teamRole of [null, "config_editor", "viewer", "member", "admin"]) {
      const note = assignableRolesNote(viewer({ orgRole: "member", teamRole }), TEAM);
      expect(note).not.toContain("config_editor");
    }
  });
});


// The fail-closed half: the ladders read rows, and a roster that has not
// answered is not a roster that says "no rank".
describe("orgStandingBlock", () => {
  it("lets the ladders decide once the roster has answered", () => {
    expect(orgStandingBlock(viewer({ orgRole: "member" }), "ready")).toBeNull();
  });

  it("lets them decide on a team with no org roster to read (D)", () => {
    // `absent` is not an unknown: there is no org ladder on such a team, so
    // nothing is withheld waiting for a read that is never made.
    expect(orgStandingBlock(viewer({ teamRole: "owner" }), "absent")).toBeNull();
  });

  it("refuses the row while the roster is in flight, and names the wait", () => {
    const d = orgStandingBlock(viewer({ teamRole: "owner" }), "loading");
    expect(d?.allowed).toBe(false);
    expect(d?.reason).toContain("still loading");
  });

  it("refuses the row when the roster failed, with a remedy and no invented role", () => {
    const d = orgStandingBlock(viewer({ teamRole: "owner" }), "failed");
    expect(d?.allowed).toBe(false);
    expect(d?.reason).toContain("could not be read");
    // Spec I: a roster that was not read names no org role — there is none
    // to name, and naming one would describe a row nobody fetched.
    for (const role of ORG_ROLES) expect(d?.reason).not.toContain(`is ${role}`);
    expect(d?.reason).toContain("platform admin");
  });

  it("refuses with the missing membership, and never a reload, when the read is REFUSED", () => {
    // Not an unknown: the server answered, and a 403 on a roster every member
    // of the org may read says this viewer holds no membership in it. The
    // words name that state and the remedy that repairs it — a reload
    // re-issues the same refusal for good.
    const d = orgStandingBlock(viewer({ teamRole: "owner" }), "refused");
    expect(d?.allowed).toBe(false);
    expect(d?.reason).toContain("You hold no membership in this team's organization");
    expect(d?.reason).toContain("an admin or owner of that organization");
    expect(d?.reason).not.toContain("reload");
    for (const role of ORG_ROLES) expect(d?.reason).not.toContain(`is ${role}`);
  });

  it("blocks nothing for a platform admin, whom no ladder reads", () => {
    for (const read of ["loading", "failed", "refused"] as RosterRead[]) {
      expect(orgStandingBlock(viewer({ platformAdmin: true }), read)).toBeNull();
    }
  });
});

describe("teamScopeFor", () => {
  it("has an org ladder exactly where there is an org roster", () => {
    // `refused` is a roster that EXISTS and was withheld, so the scope keeps
    // its org ladder: what answers that state is the gate, not the scope.
    expect(teamScopeFor("absent")).toEqual({ kind: "team", hasParentOrg: false });
    for (const read of ["ready", "loading", "failed", "refused"] as RosterRead[]) {
      expect(teamScopeFor(read)).toEqual({ kind: "team", hasParentOrg: true });
    }
  });
});

// How a roster query's own state becomes one of the five, in the one place
// that decides it for every page that mirrors these ladders.
describe("rosterReadOf", () => {
  it("answers absent where there is no such roster, whatever else is in hand", () => {
    expect(rosterReadOf({ present: false, error: null, loaded: false })).toBe("absent");
    expect(
      rosterReadOf({ present: false, error: new Error("x"), loaded: true }),
    ).toBe("absent");
  });

  it("reads the error BEFORE the data, so stale rows decide nothing", () => {
    // react-query keeps `data` through a failed refetch and sets `error`: the
    // page then holds rows and no current read of them.
    expect(rosterReadOf({ present: true, error: new Error("503"), loaded: true })).toBe(
      "failed",
    );
  });

  it("keeps a REFUSAL apart from a failure, by the status the server sent", () => {
    expect(
      rosterReadOf({
        present: true,
        error: apiErrorFrom(403, { message: "not a member of this org" }),
        loaded: false,
      }),
    ).toBe("refused");
    // Every other status is an unknown, the transport's own errors included.
    for (const status of [401, 404, 429, 500, 502]) {
      expect(
        rosterReadOf({
          present: true,
          error: apiErrorFrom(status, { message: "nope" }),
          loaded: false,
        }),
      ).toBe("failed");
    }
    expect(
      rosterReadOf({ present: true, error: new Error("API error 403: lookalike"), loaded: false }),
    ).toBe("failed");
  });

  it("answers ready only on rows, and loading until they land", () => {
    expect(rosterReadOf({ present: true, error: null, loaded: true })).toBe("ready");
    expect(rosterReadOf({ present: true, error: null, loaded: false })).toBe("loading");
  });
});

describe("canManageFromRoster", () => {
  const ready = (role: string | null) => ({ read: "ready" as RosterRead, role });
  const unread = (read: RosterRead) => ({ read, role: null });
  const none = { read: "absent" as RosterRead, role: null };

  const gate = (o: Partial<Parameters<typeof canManageFromRoster>[0]>) =>
    canManageFromRoster({
      scope: ORG,
      platformAdmin: false,
      org: ready(null),
      team: none,
      ...o,
    });

  it("admits an org admin or owner from their own roster row", () => {
    expect(gate({ org: ready("admin") })).toBe(true);
    expect(gate({ org: ready("owner") })).toBe(true);
  });

  it("refuses a standing the roster LOWERED", () => {
    // The self-demotion F1's own comment names: the server answers
    // canManageOrg's 403 on every row now.
    expect(gate({ org: ready("member") })).toBe(false);
  });

  it("admits a promotion the roster already shows, which a tree would not", () => {
    expect(gate({ org: ready("owner") })).toBe(true);
  });

  it("offers nothing while the roster it reads is in flight or failed", () => {
    for (const read of ["loading", "failed"] as RosterRead[]) {
      expect(gate({ org: unread(read) })).toBe(false);
    }
  });

  it("refuses a viewer the roster answered and carries no row for", () => {
    // The removed member: the roster ANSWERED, and has no row for them. That
    // is a standing of NONE, which is what readOwnOrgStanding answers there —
    // not an unknown an identity tree may settle. The server refuses every
    // write on such a page, and the mint beside it (measured against the
    // minting handlers: the 18 org-route cells of own_org=none are all 403
    // "org admin or owner required").
    expect(gate({ org: ready(null) })).toBe(false);
    expect(gate({ scope: TEAM, org: ready(null), team: ready(null) })).toBe(false);
    expect(
      gate({ scope: teamScopeFor("absent"), org: none, team: ready(null) }),
    ).toBe(false);
  });

  it("reads no roster as an arm of a gate that is not its own", () => {
    // A grant manages the team, never the org: the org page's gate is the org
    // row alone (its `team` input is `absent`, and an owner's grant elsewhere
    // decides nothing here).
    expect(gate({ scope: ORG, org: ready(null), team: ready("owner") })).toBe(false);
  });

  it("grants nothing from rows it holds but could not refresh", () => {
    // The reachable stale state, and the only one where the ORDER of the two
    // checks that build a RosterRead decides anything: react-query keeps
    // `data` through a failed refetch, so the viewer's own row is in hand
    // while the roster's last read failed. A row that is not a current read
    // answers no arm — read the other way round, a control enabled from
    // stale rows stays enabled for good.
    expect(gate({ org: { read: "failed", role: "owner" } })).toBe(false);
    expect(gate({ org: { read: "loading", role: "owner" } })).toBe(false);
    expect(
      gate({
        scope: TEAM,
        org: { read: "failed", role: "owner" },
        team: { read: "failed", role: "owner" },
      }),
    ).toBe(false);
  });

  it("answers an absent roster the way it answers one that has not spoken", () => {
    // `absent` is not an absence of knowledge — a team with no parent org HAS
    // no org roster — but in THIS gate neither grants anything, so the two
    // are one answer here and the arm is two-valued. Where the difference is
    // a behaviour it is read from RosterRead itself: the reason
    // orgStandingBlock gives, the ladder teamScopeFor selects.
    for (const read of ["absent", "loading", "failed"] as RosterRead[]) {
      expect(gate({ scope: TEAM, org: { read, role: null }, team: ready("member") })).toBe(
        false,
      );
      expect(gate({ scope: TEAM, org: { read, role: null }, team: ready("admin") })).toBe(
        true,
      );
    }
  });

  it("admits a platform admin, roster or no roster", () => {
    for (const read of ["ready", "loading", "failed", "absent"] as RosterRead[]) {
      expect(gate({ platformAdmin: true, org: unread(read) })).toBe(true);
    }
  });

  it("admits a team grant of admin or owner, and refuses one below", () => {
    const team = (role: string | null) =>
      gate({ scope: TEAM, org: ready("member"), team: ready(role) });
    expect(team("owner")).toBe(true);
    expect(team("admin")).toBe(true);
    expect(team("member")).toBe(false);
    expect(team("viewer")).toBe(false);
    // Off the ladder, so it manages nothing (ADR-078) — the server's
    // AtLeast(RoleAdmin) is false at rank 0.
    expect(team("config_editor")).toBe(false);
    expect(team(null)).toBe(false);
  });

  it("admits an org admin over a team they hold no grant on", () => {
    expect(
      gate({ scope: TEAM, org: ready("admin"), team: ready(null) }),
    ).toBe(true);
  });

  it("has no org arm on a team with no parent org", () => {
    expect(
      gate({ scope: teamScopeFor("absent"), org: none, team: ready("owner") }),
    ).toBe(true);
    expect(
      gate({ scope: teamScopeFor("absent"), org: none, team: ready("member") }),
    ).toBe(false);
  });

  it("waits for the team roster before answering from the org one it has", () => {
    // The org row says plain member, the grant is unread: the grant arm is
    // the one that could still say yes, so nothing is offered yet.
    expect(
      gate({ scope: TEAM, org: ready("member"), team: unread("loading") }),
    ).toBe(false);
    // And an arm that never answers (a roster whose read FAILED) is the same
    // refusal, on both rosters at once.
    expect(
      gate({ scope: TEAM, org: unread("failed"), team: unread("failed") }),
    ).toBe(false);
  });

  it("has no grant arm on a team whose org roster this viewer is REFUSED", () => {
    // The orphan grant: the refusal says they hold no membership in the
    // team's org, which is the state useCanManageTeam answers false on — a
    // drift to repair, not a permission to widen from the client. The
    // server's canManageTeam would take the write on the grant alone.
    for (const role of ["owner", "admin"]) {
      expect(
        gate({ scope: TEAM, org: unread("refused"), team: ready(role) }),
      ).toBe(false);
    }
    // An outage is not that state: there the grant still arms the gate, and
    // the org ladder refuses each control in the row.
    for (const read of ["loading", "failed"] as RosterRead[]) {
      expect(
        gate({ scope: TEAM, org: unread(read), team: ready("admin") }),
      ).toBe(true);
    }
    // A team with no parent org has no org roster to be refused, and a
    // platform admin is exempt from every roster.
    expect(
      gate({ scope: teamScopeFor("absent"), org: none, team: ready("admin") }),
    ).toBe(true);
    expect(
      gate({
        scope: TEAM,
        platformAdmin: true,
        org: unread("refused"),
        team: ready(null),
      }),
    ).toBe(true);
    // In ORG scope the refusal is the answer it already was: no row, no arm.
    expect(gate({ scope: ORG, org: unread("refused") })).toBe(false);
  });
});

// The invitation caps (K) are a DIFFERENT rule from the roster write's: the
// oracle below is the server's two reads, written from ranks.
describe("orgMirrorOfTeamRole", () => {
  it("mirrors the ladder the way identity.OrgRoleForTeamRole does", () => {
    expect(orgMirrorOfTeamRole("owner")).toBe("admin");
    expect(orgMirrorOfTeamRole("admin")).toBe("admin");
    expect(orgMirrorOfTeamRole("member")).toBe("member");
    expect(orgMirrorOfTeamRole("viewer")).toBe("member");
  });

  it("mirrors the off-ladder capability to nothing, and an unknown value too", () => {
    expect(orgMirrorOfTeamRole("config_editor")).toBeNull();
    expect(orgMirrorOfTeamRole("archon")).toBeNull();
  });
});

describe("invitableRoles", () => {
  /**
   * The server's own two guards on a mint, transcribed from its branches
   * rather than from this module's expressions — an oracle that shares the
   * mirror's shape cannot find the mirror wrong. Two places where the round-2
   * oracle did share it, and this one does not: the grant cap is exempted by
   * an org STANDING of admin or above, never by the route, and an off-ladder
   * value is not refused by the cap at all.
   *
   * `role.Valid()` (orgs_routes.go, both minting routes): the five known team
   * roles, anything else 400.
   */
  const routeAccepts = (role: string): boolean => onLadder(role, TEAM_ROLES);
  /** identity.OrgRoleForTeamRole: "" for rank 0 — config_editor, unknowns. */
  const orgMirror = (role: string): string => {
    const rank = TEAM_RANK[role] ?? 0;
    if (rank >= (TEAM_RANK.admin ?? 0)) return "admin";
    if (rank >= (TEAM_RANK.viewer ?? 0)) return "member";
    return "";
  };
  /** requireRankToInvite (pkg/server/auth_authz.go), branch for branch. */
  function serverCapAccepts(
    viewer: ViewerStanding,
    scope: WriteScope,
    role: string,
  ): boolean {
    const want = orgMirror(role);
    if (want === "" || viewer.platformAdmin) return true;
    // `orgID != ""`: the team has a parent org — the org route's invitation
    // joins a team of the org it names, so there is always one there.
    if (scope.kind === "org" || scope.hasParentOrg) {
      const standing = viewer.orgRole;
      if (!atLeast(standing, want, ORG_RANK)) return false;
      if (atLeast(standing, "admin", ORG_RANK)) return true;
    }
    return atLeast(viewer.teamRole, role, TEAM_RANK);
  }
  const serverMints = (viewer: ViewerStanding, scope: WriteScope, role: string): boolean =>
    routeAccepts(role) && serverCapAccepts(viewer, scope, role);

  it("agrees with the server's two caps on every standing the pickers can face", () => {
    for (const myOrg of ORG_VALUES) {
      for (const myTeam of TEAM_VALUES) {
        for (const platformAdmin of [false, true]) {
          const v = viewer({ platformAdmin, orgRole: myOrg, teamRole: myTeam });
          for (const scope of [TEAM, ORPHAN_TEAM, ORG]) {
            const ladder = scope.kind === "org" ? ORG_ROLES : TEAM_ROLES;
            const got = invitableRoles(v, scope, ladder);
            const want = ladder.filter((r) => serverMints(v, scope, r));
            expect(
              got,
              `${JSON.stringify({ v, scope })}`,
            ).toEqual(want);
          }
        }
      }
    }
  });

  it("measures the grant on the ORG route too, where the standing is below admin", () => {
    // requireRankToInvite exempts the grant cap on an org STANDING of admin
    // or above, whatever the route: the org route's own gate is what makes
    // this unreachable (measured through the minting handlers: every
    // org-route cell below admin answers 403 at the gate). The mirror is
    // written from the standing, so the exemption rests on the row rather
    // than on which form the mint came from.
    expect(invitableRoles(viewer({ orgRole: "member", teamRole: "viewer" }), ORG, ORG_ROLES))
      .toEqual([]);
    expect(invitableRoles(viewer({ orgRole: "member", teamRole: "owner" }), ORG, ORG_ROLES))
      .toEqual(["member"]);
    // An org admin keeps the whole picker: the mirror tops out at admin and
    // no grant is measured.
    expect(invitableRoles(viewer({ orgRole: "admin" }), ORG, ORG_ROLES)).toEqual([
      "member",
      "admin",
      "owner",
    ]);
  });

  it("caps the team admin who is a plain org member at their own grant", () => {
    // The org mirror of admin and owner is org admin, which they do not hold.
    expect(invitableRoles(viewer({ orgRole: "member", teamRole: "admin" }), TEAM, TEAM_ROLES))
      .toEqual(["config_editor", "viewer", "member"]);
  });

  it("lets an org admin mint the owner whose mirror is admin", () => {
    // The roster-write cap would refuse `owner` to an org admin; this one
    // does not, and filtering an invitation with assignableRoles would hide
    // a role the route accepts.
    expect(invitableRoles(viewer({ orgRole: "admin" }), TEAM, TEAM_ROLES)).toEqual([
      ...TEAM_ROLES,
    ]);
    expect(assignableRoles(viewer({ orgRole: "admin" }), ORG)).toEqual(["member", "admin"]);
  });

  it("caps a team owner of an org-less team at their own grant, and no mirror", () => {
    // No parent org, so no mirror to read: the grant is the whole rule (K.2).
    expect(invitableRoles(viewer({ teamRole: "admin" }), ORPHAN_TEAM, TEAM_ROLES)).toEqual([
      "config_editor",
      "viewer",
      "member",
      "admin",
    ]);
    expect(invitableRoles(viewer({ teamRole: "owner" }), ORPHAN_TEAM, TEAM_ROLES)).toEqual([
      ...TEAM_ROLES,
    ]);
  });

  it("never caps the role that mirrors up to nothing", () => {
    for (const myOrg of ORG_VALUES) {
      for (const myTeam of TEAM_VALUES) {
        for (const scope of [TEAM, ORPHAN_TEAM]) {
          expect(
            invitableRoles(viewer({ orgRole: myOrg, teamRole: myTeam }), scope, TEAM_ROLES),
          ).toContain("config_editor");
        }
      }
    }
  });

  it("mints nothing this build cannot place", () => {
    expect(invitableRoles(viewer({ orgRole: "owner" }), TEAM, ["archon", "owner"])).toEqual([
      "owner",
    ]);
  });

  it("exempts a platform admin from both caps", () => {
    expect(invitableRoles(viewer({ platformAdmin: true }), ORPHAN_TEAM, TEAM_ROLES)).toEqual([
      ...TEAM_ROLES,
    ]);
  });
});

describe("invitableRolesNote", () => {
  it("says nothing when the picker's whole ladder may be minted", () => {
    expect(invitableRolesNote(viewer({ orgRole: "admin" }), TEAM, TEAM_ROLES)).toBeNull();
    expect(invitableRolesNote(viewer({ platformAdmin: true }), TEAM, TEAM_ROLES)).toBeNull();
  });

  it("names the roles it withholds, and why an invitation cannot carry them", () => {
    const note = invitableRolesNote(
      viewer({ orgRole: "member", teamRole: "admin" }),
      TEAM,
      TEAM_ROLES,
    );
    expect(note).toContain("admin");
    expect(note).toContain("owner");
    expect(note).toContain("mirrors it up to an org membership");
  });
});

describe("reconcileRole", () => {
  it("keeps a pick the list still offers", () => {
    expect(reconcileRole(TEAM_ROLES, "member")).toBe("member");
  });

  it("falls back to the nearest role offered, not to the least-privileged one", () => {
    // The list arrives in ladder order, so the last entry is the cap the
    // pick ran into — falling back to the first would hand the operator
    // config_editor, a different capability rather than a lower rank.
    expect(reconcileRole(["config_editor", "viewer", "member", "admin"], "owner")).toBe(
      "admin",
    );
    expect(reconcileRole(["member"], "owner")).toBe("member");
  });

  it("answers null when nothing is offered, which is a form to disarm", () => {
    expect(reconcileRole([], "member")).toBeNull();
  });
});

// Deleting the team CONTAINER: the org ladder over every grant the deletion
// clears (requireRankOverTeamMembers), behind canManageOrg. The team ladder
// is not part of the rule — the deletion is an org-scoped act.
describe("canDeleteTeam", () => {
  /** requireRankOverTeamMembers, in the server's own order. */
  function serverAllowsDelete(
    v: ViewerStanding,
    memberOrgRoles: readonly (string | null)[],
    scope: WriteScope,
  ): boolean {
    if (scope.kind !== "team" || !scope.hasParentOrg) return false;
    if (v.platformAdmin) return true;
    if (!atLeast(v.orgRole, "admin", ORG_RANK)) return false;
    for (const role of memberOrgRoles) {
      if (role == null) continue; // no row: below every rung
      if (!atLeast(v.orgRole, role, ORG_RANK)) return false; // unplaceable included
    }
    return true;
  }

  const MEMBER_VALUES = [...ORG_VALUES] as const; // null included: an orphan grant

  function deleteCases(): Array<{
    v: ViewerStanding;
    roles: (string | null)[];
  }> {
    const out: Array<{ v: ViewerStanding; roles: (string | null)[] }> = [];
    for (const platformAdmin of [false, true]) {
      for (const orgRole of ORG_VALUES) {
        for (const first of MEMBER_VALUES) {
          for (const second of MEMBER_VALUES) {
            out.push({
              v: viewer({ platformAdmin, orgRole }),
              roles: [first, second],
            });
          }
        }
      }
    }
    return out;
  }

  it("answers the roster the way requireRankOverTeamMembers does, orphan teams included", () => {
    // Both scopes the server's gate orders: a team with a parent org, where
    // the platform-admin exemption lives, and one without, where its first
    // disjunct refuses everyone.
    const scopes = [TEAM, ORPHAN_TEAM];
    const wrong = deleteCases().flatMap(({ v, roles }) =>
      scopes
        .filter(
          (scope) =>
            canDeleteTeam(v, roles, scope).allowed !== serverAllowsDelete(v, roles, scope),
        )
        .map((scope) => JSON.stringify({ hasParentOrg: scope.hasParentOrg, v, roles })),
    );
    expect(wrong).toEqual([]);
    expect(deleteCases()).toHaveLength(2 * 6 * 6 * 6);
  });

  it("refuses an org admin over an org owner, and names the remedy that works", () => {
    const d = canDeleteTeam(viewer({ orgRole: "admin" }), ["owner"], TEAM);
    expect(d.allowed).toBe(false);
    expect(d.reason).toContain("an owner of this organization");
    expect(d.reason).toContain("platform admin");
  });

  it("lets an org owner delete over an equal-rank roster, own row included", () => {
    expect(
      canDeleteTeam(viewer({ orgRole: "owner" }), ["owner", "member"], TEAM).allowed,
    ).toBe(true);
  });

  it("does not let an orphan grant block, and lets an unplaceable rank refuse", () => {
    expect(canDeleteTeam(viewer({ orgRole: "admin" }), [null, "member"], TEAM).allowed).toBe(
      true,
    );
    const d = canDeleteTeam(viewer({ orgRole: "owner" }), ["archon"], TEAM);
    expect(d.allowed).toBe(false);
    expect(d.reason).toContain("cannot place");
  });

  it("refuses a grant-only team admin: the deletion is gated on canManageOrg", () => {
    const d = canDeleteTeam(
      viewer({ orgRole: "member", teamRole: "admin" }),
      ["member"],
      TEAM,
    );
    expect(d.allowed).toBe(false);
    expect(d.reason).toContain("an admin or owner standing in its organization");
  });

  it("waits for the team roster it decides from, before any rank", () => {
    for (const teamRead of ["loading", "failed", "refused"] as const) {
      const d = canDeleteTeam(viewer({ orgRole: "owner" }), ["owner"], TEAM, teamRead);
      expect(d.allowed).toBe(false);
      expect(d.reason).toContain("cannot be deleted until it is");
      if (teamRead === "loading") expect(d.reason).toContain("has not finished loading");
    }
    // The unplaceable refusal is the deletion's own sentence: the actor
    // deletes, it does not write.
    const d = canDeleteTeam(viewer({ orgRole: "owner" }), ["archon"], TEAM);
    expect(d.reason).toContain("a platform admin deletes this team");
    expect(d.reason).not.toContain("writes this row");
  });

  it("refuses a team with no parent org for everyone — the platform admin included", () => {
    expect(canDeleteTeam(viewer({ orgRole: "owner" }), [], ORPHAN_TEAM).allowed).toBe(false);
    const d = canDeleteTeam(viewer({ platformAdmin: true }), [], ORPHAN_TEAM);
    expect(d.allowed).toBe(false);
    expect(d.reason).toContain("nobody deletes it from this surface");
    // The refusal precedes the exemption: the orphan cell is refused whatever
    // the roster or the viewer holds, as the server's gate is.
    expect(canDeleteTeam(viewer({ platformAdmin: true }), ["owner"], ORPHAN_TEAM).allowed).toBe(
      false,
    );
  });
});
