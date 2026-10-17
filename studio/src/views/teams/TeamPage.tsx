import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useEffect, useId, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { Tabs } from "@/components/ui/Tabs";
import { useLocation, useParams, useSearch } from "wouter";
import { useConfirm } from "@/hooks/useConfirm";
import { useCanManageTeam } from "@/hooks/useCanManageTeam";
import {
  createInvitation,
  deleteInvitation,
  deleteTeam,
  listInvitations,
  listTeamMembers,
  putTeamMember,
  removeMember,
  updateMemberRole,
} from "@/api/byok";
import ApiKeysPanel from "@/views/account/ApiKeys";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import InviteLinkPanel from "@/components/shared/InviteLinkPanel";
import { AddExistingMemberPanel } from "@/components/shared/AddExistingMemberPanel";
import PanelLoading from "@/components/shared/PanelLoading";
import { useTeamSubject } from "@/hooks/useTenantSubject";
import { RoleSelect } from "@/components/shared/RoleSelect";
import { useAuth } from "@/auth/AuthContext";
import { TEAM_ROLES as ROLES, confirmOwnerGrant, roleLabel } from "@/lib/roles";
import {
  assignableRoles,
  assignableRolesNote,
  canDeleteTeam,
  deleteStandingBlock,
  canManageFromRoster,
  canWriteMember,
  invitableRoles,
  invitableRolesNote,
  orgStandingBlock,
  reconcileRole,
  roleOptions,
  rosterReadOf,
  teamScopeFor,
  type RosterRead,
  type ViewerStanding,
} from "@/lib/roleAuthority";
import { listOrgMembers } from "@/api/orgMembers";

import AuditTab from "./tabs/AuditTab";
import CredPoolTab from "./tabs/CredPoolTab";
import MemoryTab from "./tabs/MemoryTab";
import RoutingPolicySection from "@/views/orgs/RoutingPolicySection";
import { getTeamRoutingPolicy, putTeamRoutingPolicy } from "@/api/orgGovernance";

// SSO, Usage, members-roster and billing are ORG-level — they live on the Org
// settings page (/orgs/:id). The team page keeps the team's own administrative
// resources: who can access this team, its API keys, audit and memory. The
// integration surfaces (forges, webhooks, secrets, bot bindings, model
// providers) moved to their own top-level destination (/integrations).
type Tab = "members" | "api-keys" | "cred-pool" | "audit" | "memory" | "routing";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "members", label: "Team access" },
  { id: "api-keys", label: "API keys" },
  { id: "cred-pool", label: "Credential pool" },
  { id: "audit", label: "Audit log" },
  { id: "memory", label: "Memory" },
  { id: "routing", label: "Routing policy" },
];

export default function TeamPage() {
  const params = useParams<{ id: string }>();
  const teamID = params.id;
  // Resolved through the shared subject hook rather than out of the
  // caller's own tree: a super-admin or an org admin who holds no grant on
  // this team still gets the page, since the server would answer for them.
  const {
    subject: team,
    loading: teamLoading,
    denied,
    notFound: teamNotFound,
    error: teamError,
  } = useTeamSubject(teamID);
  const search = useSearch();
  const [, navigate] = useLocation();
  const tabFromURL = (s: string): Tab => {
    const t = new URLSearchParams(s).get("tab");
    return TABS.some((x) => x.id === t) ? (t as Tab) : "members";
  };
  const [tab, setTab] = useState<Tab>(() => tabFromURL(search));
  // Keep the tab in sync with ?tab= so a deep link selects the right tab even
  // when TeamPage is already mounted; selectTab writes it back so the URL stays
  // shareable.
  useEffect(() => {
    const t = tabFromURL(search);
    setTab((cur) => (cur === t ? cur : t));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search]);
  const selectTab = (t: Tab) => {
    setTab(t);
    navigate(`/teams/${teamID}?tab=${t}`, { replace: true });
  };

  const canManage = useCanManageTeam(teamID);

  // Breadcrumb: show "Org / Team" only when the org name actually adds
  // information. For the personal/default org (where org_name == team_name) the
  // prefix is pure redundancy ("SocialGouv / SocialGouv/socialgouv"), so we
  // collapse it to just the team name and drop the noisy /slug micro-suffix.
  //
  // It names THIS team's org, carried by the subject. Reading the active
  // org instead would label a team reached by URL with whichever tenant the
  // operator happens to be switched into.
  const orgCrumb =
    team?.orgName && team.orgName !== team.name ? team.orgName : null;
  // Same rule for the role line: the subject's role is the role on the team
  // on screen, which is a different team from the active one whenever this
  // page was reached by URL.
  const shownRole = team?.role;
  useHeaderSlot({
    left: team ? (
      <span className="text-sm font-semibold">
        {orgCrumb && <span className="text-fg-muted font-normal">{orgCrumb} / </span>}
        {team.name}
      </span>
    ) : (
      <span className="text-sm font-semibold">
        {teamLoading ? "Loading team…" : "Team not found"}
      </span>
    ),
    right: team ? (
      <span className="text-xs text-fg-muted">
        Your role: {shownRole ? roleLabel(shownRole) : "—"}
      </span>
    ) : null,
  });

  if (teamLoading) {
    return <PanelLoading label="Loading team…" />;
  }

  if (!team) {
    // Three different answers, on purpose: a refusal, an absence, and a
    // failure. Reporting an absence or an outage as a refusal tells an
    // operator they lack an access they hold, and buries the real cause.
    return (
      <div className="p-6">
        {teamError ? (
          <InlineBanner tone="danger" layout="inline">
            {teamError}
          </InlineBanner>
        ) : (
          <p className="text-sm text-fg-muted">
            {denied
              ? "You do not have access to this team."
              : teamNotFound
                ? "This team does not exist."
                : "This team could not be loaded."}
          </p>
        )}
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-6xl mx-auto p-3 sm:p-6 grid grid-cols-1 sm:grid-cols-[200px_1fr] gap-4 sm:gap-6">
        <Tabs
          variant="pill"
          value={tab}
          onValueChange={(v) => selectTab(v as Tab)}
          items={TABS.map((t) => ({ value: t.id, label: t.label }))}
          listClassName="flex sm:flex-col gap-1 flex-wrap"
          triggerClassName="sm:w-full sm:text-left"
        />

        <main>
          {tab === "members" && (
            <Members teamID={team.teamID} orgID={team.orgID} />
          )}
          {tab === "api-keys" && (
            <ApiKeysPanel team={{ id: team.teamID, name: team.name }} />
          )}
          {tab === "cred-pool" && (
            <CredPoolTab teamID={team.teamID} canManage={canManage} />
          )}
          {tab === "audit" && <AuditTab teamID={team.teamID} canManage={canManage} />}
          {tab === "memory" && <MemoryTab teamID={team.teamID} />}
          {tab === "routing" && (
            <RoutingPolicySection
              scope="team"
              id={team.teamID}
              canManage={canManage}
              getPolicy={getTeamRoutingPolicy}
              putPolicy={putTeamRoutingPolicy}
            />
          )}
        </main>
      </div>
    </div>
  );
}

function Members({
  teamID,
  orgID,
}: {
  teamID: string;
  /**
   * The team's parent org, from the team row itself. Empty is the same
   * answer as missing — a team with no parent org — and normalising it here,
   * once, is what keeps "no org roster to read" one fact: the id feeds the
   * query, the ladder selection and the gate, and a "" that reached any of
   * them would ask for the roster of an org that does not exist.
   */
  orgID: string | null;
}) {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  // The page navigates away once the team itself is gone (the delete-team
  // control below): the parent org is where its teams are listed, the account
  // page where a team with no org lives.
  const [, navigate] = useLocation();
  // Mutation failures report through setActionErr; load failures surface
  // from the queries. One banner shows whichever is current.
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState({ email: "", role: "member" });
  // Session-issued invites, newest first — tokens appear once, so a new
  // invite must not clobber a still-uncopied link.
  const [issued, setIssued] = useState<Array<{ email: string; token: string }>>([]);
  const { confirm, dialog } = useConfirm();

  const membersQuery = useQuery({
    queryKey: ["team-members", teamID],
    queryFn: () => listTeamMembers(teamID),
  });
  const invitationsQuery = useQuery({
    queryKey: ["team-invitations", teamID],
    queryFn: () => listInvitations(teamID),
  });
  // The org roster is two things at once: the candidate pool for "add an
  // existing account" (handlePutTeamMember refuses a user outside the team's
  // org, so offering only org members makes that refusal unreachable), and
  // the rows the ORG ladder compares — including the viewer's own standing,
  // which the gate below reads. That is why its `enabled` carries no gate of
  // its own: a roster asked only where the gate already said yes cannot be
  // what the gate answers from.
  // The parent org, normalised: "" is no org, like null. Every reader below
  // takes it from here.
  const parentOrgID = orgID === "" ? null : orgID;
  const orgMembersQuery = useQuery({
    queryKey: ["org-members", parentOrgID],
    queryFn: () => listOrgMembers(parentOrgID!),
    enabled: parentOrgID != null,
  });
  const members = membersQuery.data ?? [];
  const invs = invitationsQuery.data ?? [];
  // Depends on the query DATA, not on `members` — that is `… ?? []`, a
  // fresh array each render, which would re-run this on every render in
  // exactly the state where memoising was the point.
  const candidates = useMemo(() => {
    const onTeam = new Set((membersQuery.data ?? []).map((m) => m.user_id));
    return (orgMembersQuery.data ?? [])
      .filter((m) => !onTeam.has(m.user_id))
      .map((m) => ({
        user_id: m.user_id,
        email: m.email,
        name: m.name,
        orgRole: m.role as string,
      }));
  }, [orgMembersQuery.data, membersQuery.data]);
  // How the org roster answered — the one fact the org ladder, the gate and
  // the candidate pool all read. `absent` is a team with no parent org: there
  // is no org roster to read, which is why the query above is not asked for
  // one, and why no refusal here offers an org remedy (D). `refused` is a
  // viewer who holds no membership in that org, which the gate answers. The
  // other two are unknowns, and an unknown refuses rather than deciding at
  // "no rank": a roster in flight read as "nobody holds an org role" leaves
  // every control the server refuses enabled, and a failed one leaves it so
  // for good. The classification itself is the shared one, so this page and
  // the org page cannot read the same failure differently.
  // The team roster's own read: the deletion decides from its rows too, so
  // the control waits for it the way the rows wait (the org roster's
  // unknowns are the deletion's block below).
  const teamRead: RosterRead = rosterReadOf({
    present: true,
    error: membersQuery.error,
    loaded: membersQuery.data != null,
  });
  const orgRead: RosterRead = rosterReadOf({
    present: parentOrgID != null,
    error: orgMembersQuery.error,
    loaded: orgMembersQuery.data != null,
  });
  // A team-scope write answers to two ladders, and both read the ROWS the
  // server's guard reads: the viewer's grant from the team roster, their org
  // standing from the org roster. A roster that answered and carries no row
  // for them is a standing of none — the rosters are whole — and the identity
  // tree is no second opinion on it: it is fetched once at sign-in, no roster
  // write refreshes it, and believing it left a promotion on dead controls
  // and a self-demotion on live ones. It survives as the gate's last arm
  // only, below.
  const myTeamRow = (membersQuery.data ?? []).find((m) => m.user_id === user?.id);
  const myOrgRow = (orgMembersQuery.data ?? []).find((m) => m.user_id === user?.id);
  const viewer: ViewerStanding = {
    platformAdmin: !!user?.is_super_admin,
    orgRole: myOrgRow?.role ?? null,
    teamRole: myTeamRow?.role ?? null,
  };
  // A team whose parent org is not known has no org ladder at all: the grant
  // decides alone. Both facts come from the same read, so the ladder the page
  // selects and the roster state it reports cannot disagree.
  const scope = teamScopeFor(orgRead);
  // The gate the server answers with canManageTeam, from those same rows —
  // the identity tree is not one of its inputs: its "may I manage this team"
  // predates every roster write, and a row the roster has since dropped left
  // it arming a form the server answers 403 to. It closes on the one state
  // where that tree's own answer is the right one and the rows' is not: an
  // org roster REFUSED to the viewer, whose grant is then an orphan one, so
  // this tab is read-only there instead of offering a surface whose every
  // control the org ladder refuses.
  const canManage = canManageFromRoster({
    scope,
    platformAdmin: !!user?.is_super_admin,
    org: { read: orgRead, role: myOrgRow?.role ?? null },
    team: {
      read: rosterReadOf({
        present: true,
        error: membersQuery.error,
        loaded: membersQuery.data != null,
      }),
      role: myTeamRow?.role ?? null,
    },
  });
  // Why nothing the org ladder decides is offered when the org roster did not
  // give its rows — the reason that replaces the raw refusal, in the row for
  // every control that ladder feeds, and on the page itself where the gate
  // above closed instead.
  const standingBlocked = orgStandingBlock(viewer, orgRead);
  // Each row's ORG role, which the org ladder compares against the standing
  // above. Where the roster could not be read nobody carries one — which is
  // why `standingBlocked` refuses the row rather than letting the ladder
  // compare against nothing, and why no reason here names an org role: there
  // is none to name (I).
  const orgRoleByUser = useMemo(() => {
    const byUser = new Map<string, string>();
    for (const row of orgMembersQuery.data ?? []) byUser.set(row.user_id, row.role);
    return byUser;
  }, [orgMembersQuery.data]);
  const offeredRoles = assignableRoles(viewer, scope);
  const rolesNote = assignableRolesNote(viewer, scope);
  // The PUT the candidate pool feeds takes the same guard as the rows below:
  // a candidate the org ladder puts above the viewer is refused there, so
  // offering them is a dead control with a 403 behind it. The role this panel
  // writes is capped separately, by `offeredRoles`.
  const placeable = candidates.filter(
    (c) =>
      (standingBlocked ??
        canWriteMember(viewer, { orgRole: c.orgRole, teamRole: null }, scope)).allowed,
  );
  // Deleting the team itself is a container write over every grant it holds
  // (DELETE /api/teams/{id}): the server refuses an author over a member
  // their org standing does not cover, before any revoke. The rows it reads
  // are the ones the table above already joins — each member's org role from
  // the org roster — and the whole-surface unknowns are the same
  // `standingBlocked` answer the rows compose first.
  const memberOrgRoles = useMemo(
    () => (membersQuery.data ?? []).map((m) => orgRoleByUser.get(m.user_id) ?? null),
    [membersQuery.data, orgRoleByUser],
  );
  const deletion =
    deleteStandingBlock(viewer, orgRead) ??
    canDeleteTeam(viewer, memberOrgRoles, scope, teamRead);
  // Why a candidate is missing is not one reason. Where the org roster has
  // not answered, `placeable` withholds EVERY one of them — the rows can
  // still be in hand, since react-query keeps them through a failed refetch,
  // and the page has declared them unreadable — so the refusal is "the roster
  // could not be read", which `emptyMessage` below states and no rank
  // sentence may: no rank was compared, and the org admin a rank sentence
  // sends the operator to cannot help. Only what the org LADDER withheld is
  // counted here (I).
  const withheldByRank =
    standingBlocked == null ? candidates.length - placeable.length : 0;
  // The roles an invitation may carry are capped by a DIFFERENT rule from the
  // roster write's: the role's org mirror against the viewer's org standing,
  // and — where their authority over the team is their own grant — the role
  // against that grant (K).
  const inviteRoles = invitableRoles(viewer, scope, ROLES);
  const inviteRolesNote = invitableRolesNote(viewer, scope, ROLES);
  // What the form will actually send, which is what the select must show.
  // `null` is a list with nothing in it, which this page cannot produce —
  // config_editor is capped by neither rule, so the team ladder always keeps
  // it — but the shape belongs to the form rather than to the page: the same
  // expression guards the org invitation form and AddExistingMemberPanel,
  // whose list its caller's own standing CAN empty.
  const inviteRole = reconcileRole(inviteRoles, draft.role);
  // The ids of this section's controls and of the explanations they name: the
  // invitation form's two (why nothing is offered, why the ladder offered is
  // short) and, below, one per roster row. All of them unique to this instance
  // — a module constant collides the day a second Members section shares a
  // page, and `getElementById` resolves the FIRST, so a control would describe
  // the other section's text.
  const sectionID = useId();
  const inviteEmailID = `${sectionID}-invite-email`;
  const inviteRoleID = `${sectionID}-invite-role`;
  const standingReasonID = `${sectionID}-invite-standing`;
  const inviteRolesNoteID = `${sectionID}-invite-roles`;
  const deleteReasonID = `${sectionID}-delete-team`;
  // Where the invitation select and its submit send a screen reader: the
  // refusal when there is one, the short-ladder note when there is one, both
  // when both apply.
  const inviteDescribedBy =
    [standingBlocked ? standingReasonID : null, inviteRolesNote ? inviteRolesNoteID : null]
      .filter((id) => id != null)
      .join(" ") || undefined;
  const fetching =
    membersQuery.isFetching || invitationsQuery.isFetching || orgMembersQuery.isFetching;
  // The org roster's failure is a load failure of this page like the other
  // two: it is the reason half its controls are refused, so it is reported
  // where a reader looks for one rather than only in the rows. Its REFUSAL is
  // not a failure of this page — the server answered, and what it answered is
  // stated in full below, in the words the operator can act on — so the
  // transport's own "API error 403" line does not double it.
  const loadError =
    membersQuery.error ??
    invitationsQuery.error ??
    (orgRead === "refused" ? null : orgMembersQuery.error);
  const err =
    actionErr ?? (loadError && !fetching ? errorMessage(loadError) : null);

  // Post-mutation refresh: invalidate both lists (and clear any stale
  // mutation error, as the old reload() did).
  const reload = () => {
    setActionErr(null);
    void queryClient.invalidateQueries({ queryKey: ["team-members", teamID] });
    void queryClient.invalidateQueries({ queryKey: ["team-invitations", teamID] });
  };

  const invite = async (ev: React.FormEvent) => {
    ev.preventDefault();
    // Sent at the role the select SHOWS — the cap narrows when the roster it
    // reads lands, and the held pick must not outlive what was on screen.
    if (inviteRole == null || standingBlocked != null) return;
    // An invitation writes a role from the same ladder the add form does,
    // `owner` included — it is the slower door to the same handover, not a
    // different act. Guarding only the direct grant left the sentence
    // "hand over ownership in one un-prompted click" true, by this route.
    if (!(await confirmOwnerGrant(confirm, inviteRole))) return;
    setBusy(true);
    setActionErr(null);
    try {
      const r = await createInvitation(teamID, { email: draft.email, role: inviteRole });
      setIssued((list) => [{ email: draft.email, token: r.token }, ...list]);
      setDraft({ email: "", role: "member" });
      reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const cancel = async (id: string) => {
    const ok = await confirm({
      title: "Cancel invitation?",
      message: "Cancel this invitation?",
      confirmLabel: "Cancel invitation",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteInvitation(teamID, id);
      reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    }
  };

  // The demotion / owner-handover prompt lives in RoleSelect, so every
  // surface that writes a membership role inherits it instead of
  // re-deriving it.
  const setRole = async (userID: string, role: string) => {
    try {
      await updateMemberRole(teamID, userID, role);
      reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    }
  };

  const kick = async (userID: string) => {
    const ok = await confirm({
      title: "Remove member?",
      message:
        "Remove this member from the team? Where they hold no membership in the team's organization, the pending invitations they addressed to their own address in this team are revoked with the grant — an invitation link already sent stops working.",
      confirmLabel: "Remove member",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await removeMember(teamID, userID);
      reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    }
  };

  const destroy = async () => {
    const ok = await confirm({
      title: "Delete team?",
      message:
        "Delete this team? Every member's access to it is revoked with it. A team that still holds data — runs, keys, integrations — is refused by the server until it is emptied.",
      confirmLabel: "Delete team",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteTeam(teamID);
      void queryClient.invalidateQueries({ queryKey: ["team-members", teamID] });
      navigate(orgID ? `/orgs/${orgID}` : "/account");
    } catch (e) {
      setActionErr(errorMessage(e));
    }
  };

  return (
    <div className="space-y-6">
      {dialog}
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {/* Why this tab offers nothing to a viewer the org roster refuses: the
          gate above closed, so there is no control left to carry the reason,
          and a surface that says nothing reads as a broken page. The words are
          the ladder's own (`orgStandingBlock`), and they are null for the one
          viewer no roster decides for — a platform admin, whom this read never
          refuses. */}
      {orgRead === "refused" && standingBlocked && (
        <InlineBanner tone="warning" layout="inline">
          {standingBlocked.reason}
        </InlineBanner>
      )}

      {/* No org roster, no candidate pool: a team with no parent org has
          nobody to place from, and `orgRead` is where that is read — the same
          fact the panel's loading state below comes from, so the two cannot
          drift apart. (A disabled react-query is `isPending` forever, which
          is why `isPending` is not that fact.) */}
      {canManage && orgRead !== "absent" && (
        <AddExistingMemberPanel
          title="Add someone who already has an account"
          description={
            <>
              Candidates are this team&apos;s organization members who are not
              on the team yet. Someone outside the organization is reached by
              the invitation below — an org membership is the identity
              boundary a team grant sits inside.
              {withheldByRank > 0 && (
                <>
                  {" "}
                  {withheldByRank} of them {withheldByRank === 1 ? "is" : "are"}{" "}
                  not offered: their role in this organization is above your
                  own standing there, and the grant would be refused — an
                  admin or owner of that organization, or a platform admin,
                  places them.
                </>
              )}
            </>
          }
          candidates={placeable}
          loading={orgRead === "loading"}
          busy={busy}
          roles={offeredRoles}
          defaultRole="member"
          queryPlaceholder="Pick an org member…"
          confirm={confirm}
          emptyMessage={
            orgRead === "failed"
              ? "This organization's roster could not be read, so no candidate is offered: reload the page, or use the invitation below."
              : "Every member of this organization is already on the team."
          }
          addLabel="Add to team"
          onAdd={async (userID, role) => {
            setActionErr(null);
            setBusy(true);
            try {
              await putTeamMember(teamID, userID, role);
              reload();
            } catch (e) {
              setActionErr(errorMessage(e));
            } finally {
              setBusy(false);
            }
          }}
        />
      )}

      {canManage && (
        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-3">
          <h3 className="font-medium">Invite a member</h3>
          <form onSubmit={invite} className="flex gap-2 items-end">
            <div className="flex-1">
              <label htmlFor={inviteEmailID} className="sr-only">
                Email
              </label>
              <Input
                size="md"
                type="email"
                id={inviteEmailID}
                placeholder="email@example.com"
                value={draft.email}
                onChange={(e) => setDraft({ ...draft, email: e.target.value })}
                required
              />
            </div>
            <div>
              <label htmlFor={inviteRoleID} className="sr-only">
                Role
              </label>
              <Select
                size="md"
                id={inviteRoleID}
                value={inviteRole ?? ""}
                disabled={inviteRole == null || standingBlocked != null}
                aria-describedby={inviteDescribedBy}
                onChange={(e) => setDraft({ ...draft, role: e.target.value })}
              >
                {inviteRoles.map((r) => (
                  <option key={r} value={r}>
                    {roleLabel(r)}
                  </option>
                ))}
              </Select>
            </div>
            <Button
              variant="primary"
              type="submit"
              loading={busy}
              disabled={inviteRole == null || standingBlocked != null}
              aria-describedby={inviteDescribedBy}
            >
              Send invite
            </Button>
          </form>
          {standingBlocked && (
            <p id={standingReasonID} className="text-caption text-fg-subtle">
              {standingBlocked.reason}
            </p>
          )}
          <p className="text-caption text-fg-subtle">
            Invitees new to the organization get org membership automatically
            when they accept.
          </p>
          {inviteRolesNote && (
            <p id={inviteRolesNoteID} className="text-caption text-fg-subtle">
              {inviteRolesNote}
            </p>
          )}
          {issued.map((inv) => (
            <InviteLinkPanel
              key={inv.token}
              email={inv.email}
              token={inv.token}
              onDismiss={() =>
                setIssued((list) => list.filter((x) => x.token !== inv.token))
              }
            />
          ))}
        </section>
      )}

      <section>
        <h3 className="font-medium mb-2">Members</h3>
        <Table caption="Team members">
          <THead>
            <Th>Email</Th>
            <Th>Name</Th>
            <Th>Role</Th>
            <Th align="right" srLabel="Actions" />
          </THead>
          <TBody>
            {members.map((m) => {
              const write =
                standingBlocked ??
                canWriteMember(
                  viewer,
                  { orgRole: orgRoleByUser.get(m.user_id) ?? null, teamRole: m.role },
                  scope,
                );
              // The reason is TEXT in the row, named by both controls it
              // explains: a disabled control leaves the tab order, so a
              // `title` on it reaches a mouse and nobody else. Prefixed like
              // the form's ids, and for the same reason: two Members sections
              // in one document would otherwise carry this id twice.
              const reasonID = `${sectionID}-team-member-rank-${m.user_id}`;
              return (
                <Tr key={m.user_id}>
                  <Td>{m.email ?? m.user_id}</Td>
                  <Td>{m.name ?? "—"}</Td>
                  <Td>
                    {canManage ? (
                      <>
                        <RoleSelect
                          value={m.role}
                          roles={
                            write.allowed ? roleOptions(offeredRoles, m.role) : [m.role]
                          }
                          disabled={!write.allowed}
                          describedBy={write.allowed ? undefined : reasonID}
                          ariaLabel={`Role for ${m.email ?? m.user_id}`}
                          confirmChangeFrom={m.role}
                          confirm={confirm}
                          onChange={(role) => setRole(m.user_id, role)}
                        />
                        {write.reason && (
                          <p id={reasonID} className="text-caption text-fg-subtle mt-1">
                            {write.reason}
                          </p>
                        )}
                      </>
                    ) : (
                      roleLabel(m.role)
                    )}
                  </Td>
                  <Td align="right">
                    {canManage && (
                      <Button
                        variant="danger"
                        size="sm"
                        disabled={!write.allowed}
                        aria-describedby={write.allowed ? undefined : reasonID}
                        onClick={() => kick(m.user_id)}
                      >
                        Remove
                      </Button>
                    )}
                  </Td>
                </Tr>
              );
            })}
          </TBody>
        </Table>
        {canManage && rolesNote && (
          <p className="text-caption text-fg-subtle mt-2">{rolesNote}</p>
        )}
      </section>

      <section>
        <h3 className="font-medium mb-2">Pending invitations</h3>
        {invs.length === 0 ? (
          <div className="text-fg-muted text-sm">None.</div>
        ) : (
          <Table caption="Pending team invitations">
            <THead>
              <Th>Email</Th>
              <Th>Role</Th>
              <Th>Expires</Th>
              <Th align="right" srLabel="Actions" />
            </THead>
            <TBody>
              {invs.map((i) => (
                <Tr key={i.id}>
                  <Td>{i.email}</Td>
                  <Td>{roleLabel(i.role)}</Td>
                  <Td className="text-fg-muted">
                    {formatDateTime(i.expires_at)}
                  </Td>
                  <Td align="right">
                    {canManage && (
                      <Button
                        variant="danger"
                        size="sm"
                        onClick={() => cancel(i.id)}
                      >
                        Cancel
                      </Button>
                    )}
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        )}
      </section>

      {canManage && (
        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-3">
          <h3 className="font-medium">Delete team</h3>
          <p className="text-caption text-fg-subtle">
            Deletes this empty team and revokes every member&apos;s access to
            it. A team that still holds data is refused by the server until it
            is emptied.
          </p>
          <Button
            variant="danger"
            size="sm"
            loading={busy}
            disabled={!deletion.allowed}
            aria-describedby={deletion.allowed ? undefined : deleteReasonID}
            onClick={() => void destroy()}
          >
            Delete team
          </Button>
          {!deletion.allowed && (
            <p id={deleteReasonID} className="text-caption text-fg-subtle">
              {deletion.reason}
            </p>
          )}
        </section>
      )}
    </div>
  );
}
