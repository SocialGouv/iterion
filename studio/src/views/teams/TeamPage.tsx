import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useEffect, useMemo, useState } from "react";
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
import { useAuth } from "@/auth/AuthContext";
import {
  createInvitation,
  deleteInvitation,
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
import { useTeamSubject } from "@/hooks/useTenantSubject";
import { TEAM_ROLES as ROLES, isDemotion, roleLabel } from "@/lib/roles";
import { listOrgMembers } from "@/api/orgMembers";

import AuditTab from "./tabs/AuditTab";
import CredPoolTab from "./tabs/CredPoolTab";
import MemoryTab from "./tabs/MemoryTab";

// SSO, Usage, members-roster and billing are ORG-level — they live on the Org
// settings page (/orgs/:id). The team page keeps the team's own administrative
// resources: who can access this team, its API keys, audit and memory. The
// integration surfaces (forges, webhooks, secrets, bot bindings, model
// providers) moved to their own top-level destination (/integrations).
type Tab = "members" | "api-keys" | "cred-pool" | "audit" | "memory";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "members", label: "Team access" },
  { id: "api-keys", label: "API keys" },
  { id: "cred-pool", label: "Credential pool" },
  { id: "audit", label: "Audit log" },
  { id: "memory", label: "Memory" },
];

export default function TeamPage() {
  const params = useParams<{ id: string }>();
  const teamID = params.id;
  const { activeOrg, activeRole } = useAuth();
  // Resolved through the shared subject hook rather than out of the
  // caller's own tree: a super-admin or an org admin who holds no grant on
  // this team still gets the page, since the server would answer for them.
  const { subject: team, loading: teamLoading, denied } = useTeamSubject(teamID);
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
  // The org crumb only applies to a team inside the ACTIVE org; for one
  // reached from elsewhere it would name the wrong parent.
  const showOrgCrumb =
    !!activeOrg && team != null && team.isMember && activeOrg.org_name !== team.name;
  // The role line describes the team on screen: activeRole is the role on
  // the ACTIVE team, which is a different team whenever this page was
  // reached by URL.
  const shownRole = team?.isMember ? (team.role ?? activeRole) : team?.role;
  useHeaderSlot({
    left: team ? (
      <span className="text-sm font-semibold">
        {showOrgCrumb && (
          <span className="text-fg-muted font-normal">{activeOrg!.org_name} / </span>
        )}
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
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Loading team…</p>
      </div>
    );
  }

  if (!team) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">
          {denied
            ? "You do not have access to this team."
            : "This team could not be found."}
        </p>
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
            <Members teamID={team.teamID} orgID={team.orgID} canManage={canManage} />
          )}
          {tab === "api-keys" && (
            <ApiKeysPanel team={{ id: team.teamID, name: team.name }} />
          )}
          {tab === "cred-pool" && (
            <CredPoolTab teamID={team.teamID} canManage={canManage} />
          )}
          {tab === "audit" && (
            <AuditTab teamID={team.teamID} ownerOrgID={team.orgID} canManage={canManage} />
          )}
          {tab === "memory" && <MemoryTab teamID={team.teamID} />}
        </main>
      </div>
    </div>
  );
}

function Members({
  teamID,
  orgID,
  canManage,
}: {
  teamID: string;
  orgID: string | null;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
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
  // The org roster is the candidate pool for "add an existing account":
  // handlePutTeamMember refuses a user outside the team's org (422), so
  // offering only org members makes that refusal unreachable from here.
  const orgMembersQuery = useQuery({
    queryKey: ["org-members", orgID],
    queryFn: () => listOrgMembers(orgID!),
    enabled: canManage && orgID != null,
  });
  const members = membersQuery.data ?? [];
  const invs = invitationsQuery.data ?? [];
  const candidates = useMemo(() => {
    const onTeam = new Set(members.map((m) => m.user_id));
    return (orgMembersQuery.data ?? [])
      .filter((m) => !onTeam.has(m.user_id))
      .map((m) => ({ user_id: m.user_id, email: m.email, name: m.name }));
  }, [orgMembersQuery.data, members]);
  const fetching = membersQuery.isFetching || invitationsQuery.isFetching;
  const loadError = membersQuery.error ?? invitationsQuery.error;
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
    setBusy(true);
    setActionErr(null);
    try {
      const r = await createInvitation(teamID, draft);
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

  const setRole = async (userID: string, currentRole: string, role: string) => {
    if (role === currentRole) return;
    // Confirm demotions and any change touching "owner" — these are the
    // role edits that can lock someone out or hand over control. Routine
    // promotions (e.g. member → admin) apply without a prompt.
    if (isDemotion(currentRole, role) || currentRole === "owner" || role === "owner") {
      const ok = await confirm({
        title: "Change member role?",
        message: `Change this member from "${currentRole}" to "${role}"? This takes effect immediately.`,
        confirmLabel: "Change role",
        confirmVariant: "danger",
      });
      // On cancel the controlled <Select> re-renders back to the server
      // role (confirm's state change forces a re-render), so no manual revert.
      if (!ok) return;
    }
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
      message: "Remove this member from the team?",
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

  return (
    <div className="space-y-6">
      {dialog}
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {canManage && orgID && (
        <AddExistingMemberPanel
          title="Add someone who already has an account"
          description={
            <>
              Candidates are this team&apos;s organization members who are not
              on the team yet. Someone outside the organization is reached by
              the invitation below — an org membership is the identity
              boundary a team grant sits inside.
            </>
          }
          candidates={candidates}
          loading={orgMembersQuery.isPending}
          busy={busy}
          roles={ROLES}
          defaultRole="member"
          roleLabel={roleLabel}
          emptyMessage="Every member of this organization is already on the team."
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
              <label htmlFor="invite-email" className="sr-only">
                Email
              </label>
              <Input
                size="md"
                type="email"
                id="invite-email"
                placeholder="email@example.com"
                value={draft.email}
                onChange={(e) => setDraft({ ...draft, email: e.target.value })}
                required
              />
            </div>
            <div>
              <label htmlFor="invite-role" className="sr-only">
                Role
              </label>
              <Select
                size="md"
                id="invite-role"
                value={draft.role}
                onChange={(e) => setDraft({ ...draft, role: e.target.value })}
              >
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    {roleLabel(r)}
                  </option>
                ))}
              </Select>
            </div>
            <Button variant="primary" type="submit" loading={busy}>
              Send invite
            </Button>
          </form>
          <p className="text-caption text-fg-subtle">
            Invitees new to the organization get org membership automatically
            when they accept.
          </p>
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
            {members.map((m) => (
              <Tr key={m.user_id}>
                <Td>{m.email ?? m.user_id}</Td>
                <Td>{m.name ?? "—"}</Td>
                <Td>
                  {canManage ? (
                    <Select
                      value={m.role}
                      onChange={(e) => setRole(m.user_id, m.role, e.target.value)}
                      aria-label={`Role for ${m.email ?? m.user_id}`}
                    >
                      {ROLES.map((r) => (
                        <option key={r} value={r}>
                          {roleLabel(r)}
                        </option>
                      ))}
                    </Select>
                  ) : (
                    roleLabel(m.role)
                  )}
                </Td>
                <Td align="right">
                  {canManage && (
                    <Button
                      variant="danger"
                      size="sm"
                      onClick={() => kick(m.user_id)}
                    >
                      Remove
                    </Button>
                  )}
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
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
    </div>
  );
}
