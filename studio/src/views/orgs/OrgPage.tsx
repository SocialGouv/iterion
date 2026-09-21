import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useParams, useSearch } from "wouter";

import { hasOrgRole, useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Spinner } from "@/components/ui/Spinner";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { Tabs } from "@/components/ui/Tabs";
import { useConfirm } from "@/hooks/useConfirm";
import { useDebounce } from "@/hooks/useDebounce";
import { useOrgSubject } from "@/hooks/useTenantSubject";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import InviteLinkPanel from "@/components/shared/InviteLinkPanel";
import { AddExistingMemberPanel } from "@/components/shared/AddExistingMemberPanel";

import {
  type OrgInvitationView,
  type OrgMemberView,
  createOrgInvitation,
  deleteOrgInvitation,
  listOrgInvitations,
  listOrgMembers,
  putOrgMember,
  removeOrgMember,
  updateOrgMemberRole,
} from "@/api/orgMembers";
import { listAdminUsers } from "@/api/admin";
import { type OrgRole } from "@/api/auth";
import { listOrgTeamSummaries } from "@/api/orgGovernance";
import { createOrgTeam } from "@/api/orgs";
import { fmtBytes, fmtUSD, getOrgUsage } from "@/api/usage";
import GovernanceTab from "@/views/orgs/GovernanceTab";
import SSOTab from "@/views/teams/tabs/SSOTab";
import UsageTab from "@/views/teams/tabs/UsageTab";
import AuditTab from "@/views/teams/tabs/AuditTab";

const ORG_ROLES: OrgRole[] = ["member", "admin", "owner"];

type Tab = "members" | "teams" | "sso" | "usage" | "audit" | "billing" | "governance";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "members", label: "Members + invitations" },
  { id: "teams", label: "Teams" },
  { id: "sso", label: "SSO" },
  { id: "usage", label: "Usage" },
  { id: "audit", label: "Audit log" },
  { id: "billing", label: "Plan + quotas" },
  { id: "governance", label: "Governance" },
];

export default function OrgPage() {
  const params = useParams<{ id: string }>();
  const orgID = params.id;
  const { activeOrgRole, user } = useAuth();
  // Resolved through the shared subject hook rather than out of the
  // caller's own tree: a super-admin who is not a member still gets the
  // page, since every API behind it would answer for them.
  const { subject: org, loading: orgLoading, denied } = useOrgSubject(orgID);
  const search = useSearch();
  const [, navigate] = useLocation();
  const tabFromURL = (s: string): Tab => {
    const t = new URLSearchParams(s).get("tab");
    return TABS.some((x) => x.id === t) ? (t as Tab) : "members";
  };
  const [tab, setTab] = useState<Tab>(() => tabFromURL(search));
  useEffect(() => {
    const t = tabFromURL(search);
    setTab((cur) => (cur === t ? cur : t));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search]);
  const selectTab = (t: Tab) => {
    setTab(t);
    navigate(`/orgs/${orgID}?tab=${t}`, { replace: true });
  };

  // An org admin/owner (or a super-admin) may mutate.
  // activeOrgRole describes the ACTIVE org, so it only stands in for a
  // subject the caller is actually a member of; for an org reached by URL
  // it would name a different tenant's role.
  const orgRole = (org?.role || (org?.isMember ? activeOrgRole : null) || null) as OrgRole | null;
  const canManage = !!user?.is_super_admin || hasOrgRole(orgRole, "admin");

  useHeaderSlot({
    left: org ? (
      <span className="text-sm font-semibold">
        {org.name}
        <span className="ml-2 text-xs text-fg-muted font-normal">/{org.slug}</span>
      </span>
    ) : (
      <span className="text-sm font-semibold">
        {orgLoading ? "Loading organization…" : "Organization not found"}
      </span>
    ),
    right: org ? (
      <span className="text-xs text-fg-muted">
        Your org role: {orgRole ?? (user?.is_super_admin ? "super-admin" : "—")}
      </span>
    ) : null,
  });

  if (orgLoading) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Loading organization…</p>
      </div>
    );
  }

  if (!org) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">
          {denied
            ? "You do not have access to this organization."
            : "This organization could not be found."}
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
          {tab === "members" && <OrgMembers orgID={org.orgID} canManage={canManage} />}
          {tab === "teams" && <OrgTeams orgID={org.orgID} canManage={canManage} />}
          {tab === "sso" && <SSOTab teamID={org.orgID} canManage={canManage} />}
          {tab === "usage" && <UsageTab orgID={org.orgID} />}
          {tab === "audit" && <AuditTab orgID={org.orgID} canManage={canManage} />}
          {tab === "billing" && <OrgBilling orgID={org.orgID} />}
          {tab === "governance" && <GovernanceTab orgID={org.orgID} canManage={canManage} />}
        </main>
      </div>
    </div>
  );
}

function OrgMembers({ orgID, canManage }: { orgID: string; canManage: boolean }) {
  const { user } = useAuth();
  // The org's teams come from the server, not from the caller's identity
  // tree: an invitation dropdown built from the tree is empty for a
  // super-admin who is not a member — which reads as "this org has no
  // team" rather than as "you were not asked".
  const orgTeamsQuery = useQuery({
    queryKey: ["org-team-summaries", orgID],
    queryFn: () => listOrgTeamSummaries(orgID),
  });
  const orgTeams = useMemo(
    () =>
      (orgTeamsQuery.data ?? []).map((t) => ({
        team_id: t.id,
        team_name: t.name,
      })),
    [orgTeamsQuery.data],
  );
  const queryClient = useQueryClient();
  const membersQuery = useQuery<OrgMemberView[]>({
    queryKey: ["org-members", orgID],
    queryFn: () => listOrgMembers(orgID),
  });
  // Invitations are admin-only server-side — don't fire the doomed request
  // for a plain member (the manual fetch resolved [] in that case).
  const invitationsQuery = useQuery<OrgInvitationView[]>({
    queryKey: ["org-invitations", orgID],
    queryFn: () => listOrgInvitations(orgID),
    enabled: canManage,
  });
  const members = membersQuery.data ?? [];
  const invs = invitationsQuery.data ?? [];
  // Mutation failures share the banner with either fetch error (mutation
  // wins, like the old single slot). They're tagged with their scope so a
  // stale one never outlives an org switch — the manual reload cleared it
  // there.
  const errScope = `${orgID}:${canManage}`;
  const [mutErrTag, setMutErrTag] = useState<{ scope: string; msg: string } | null>(null);
  const setMutErr = (msg: string | null) =>
    setMutErrTag(msg === null ? null : { scope: errScope, msg });
  const mutErr = mutErrTag && mutErrTag.scope === errScope ? mutErrTag.msg : null;
  const err =
    mutErr ??
    (membersQuery.error
      ? errorMessage(membersQuery.error)
      : invitationsQuery.error
        ? errorMessage(invitationsQuery.error)
        : null);
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState({ email: "", role: "member", team_id: "" });
  // Every invite issued in this session, newest first — tokens appear
  // once server-side, so a second invite must not clobber the first's
  // still-uncopied link.
  const [issued, setIssued] = useState<Array<{ email: string; token: string }>>([]);
  // Super-admin candidate lookup: the server matches an email PREFIX (or an
  // exact id), so this is a query rather than a list to filter. Debounced,
  // and never fired for a non-super-admin — the panel is not rendered for
  // them and the endpoint would refuse anyway.
  const [candidateSearch, setCandidateSearch] = useState("");
  const debouncedCandidateSearch = useDebounce(candidateSearch.trim(), 250);
  const candidateQuery = useQuery({
    queryKey: ["admin-users", "org-candidates", debouncedCandidateSearch],
    queryFn: () => listAdminUsers({ q: debouncedCandidateSearch, limit: 20 }),
    enabled: !!user?.is_super_admin && debouncedCandidateSearch !== "",
  });
  const candidates = useMemo(() => {
    const already = new Set(members.map((m) => m.user_id));
    return (candidateQuery.data?.users ?? [])
      .filter((u) => !already.has(u.id))
      .map((u) => ({ user_id: u.id, email: u.email, name: u.name }));
  }, [candidateQuery.data, members]);
  const { confirm, dialog } = useConfirm();

  // Post-mutation refresh: clear the shared error slot and refetch both lists.
  const reload = () => {
    setMutErr(null);
    void queryClient.invalidateQueries({ queryKey: ["org-members", orgID] });
    void queryClient.invalidateQueries({ queryKey: ["org-invitations", orgID] });
  };

  const invite = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setBusy(true);
    setMutErr(null);
    try {
      const r = await createOrgInvitation(orgID, {
        email: draft.email,
        role: draft.role,
        team_id: draft.team_id || undefined,
      });
      setIssued((list) => [{ email: draft.email, token: r.token }, ...list]);
      setDraft({ email: "", role: "member", team_id: draft.team_id });
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
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
      await deleteOrgInvitation(orgID, id);
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    }
  };

  const setRole = async (userID: string, currentRole: OrgRole, role: OrgRole) => {
    if (role === currentRole) return;
    const demotion = ORG_ROLES.indexOf(role) < ORG_ROLES.indexOf(currentRole);
    if (demotion || currentRole === "owner" || role === "owner") {
      const ok = await confirm({
        title: "Change org role?",
        message: `Change this member from "${currentRole}" to "${role}"? This takes effect immediately.`,
        confirmLabel: "Change role",
        confirmVariant: "danger",
      });
      if (!ok) return;
    }
    try {
      await updateOrgMemberRole(orgID, userID, role);
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    }
  };

  const kick = async (userID: string) => {
    const ok = await confirm({
      title: "Remove member?",
      message: "Remove this member from the organization? They lose access to every team in it.",
      confirmLabel: "Remove member",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await removeOrgMember(orgID, userID);
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
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

      {/* Placing an EXISTING account in an org is super-admin only, on
          purpose: an org admin resolving arbitrary addresses would be able
          to probe which emails have an account on the platform. They keep
          the invitation below, which tells them nothing. */}
      {user?.is_super_admin && (
        <AddExistingMemberPanel
          title="Add an account that already exists"
          description={
            <>
              Super-admin only. Search by email prefix, or paste a user id. An
              org admin adds people through the invitation below — it works
              whether or not the address already has an account.
            </>
          }
          candidates={candidates}
          loading={candidateQuery.isFetching}
          busy={busy}
          roles={ORG_ROLES}
          defaultRole="member"
          emptyMessage={
            candidateSearch.trim() === ""
              ? "Type an email prefix to find an account."
              : "No account matches, or they are already a member."
          }
          addLabel="Add to org"
          onQueryChange={setCandidateSearch}
          onAdd={async (userID, role) => {
            setMutErr(null);
            setBusy(true);
            try {
              await putOrgMember(orgID, userID, role as OrgRole);
              reload();
            } catch (e) {
              setMutErr(errorMessage(e));
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
              <label htmlFor="org-invite-email" className="sr-only">
                Email
              </label>
              <Input
                size="md"
                type="email"
                id="org-invite-email"
                placeholder="email@example.com"
                value={draft.email}
                onChange={(e) => setDraft({ ...draft, email: e.target.value })}
                required
              />
            </div>
            <div>
              <label htmlFor="org-invite-role" className="sr-only">
                Role
              </label>
              <Select
                size="md"
                id="org-invite-role"
                value={draft.role}
                onChange={(e) => setDraft({ ...draft, role: e.target.value })}
              >
                {ORG_ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </Select>
            </div>
            {orgTeams.length > 0 && (
              <div>
                <label htmlFor="org-invite-team" className="sr-only">
                  Also add to team
                </label>
                <Select
                  size="md"
                  id="org-invite-team"
                  value={draft.team_id}
                  onChange={(e) => setDraft({ ...draft, team_id: e.target.value })}
                  title="Also grant access to a team — otherwise the invitee joins the org with no team and needs a second invite"
                >
                  <option value="">No team (org only)</option>
                  {orgTeams.map((t) => (
                    <option key={t.team_id} value={t.team_id}>
                      + team: {t.team_name}
                    </option>
                  ))}
                </Select>
              </div>
            )}
            <Button variant="primary" type="submit" loading={busy}>
              Send invite
            </Button>
          </form>
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
        <Table caption="Organization members">
          <THead>
            <Th>Email</Th>
            <Th>Name</Th>
            <Th>Org role</Th>
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
                      onChange={(e) => setRole(m.user_id, m.role, e.target.value as OrgRole)}
                      aria-label={`Org role for ${m.email ?? m.user_id}`}
                    >
                      {ORG_ROLES.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </Select>
                  ) : (
                    m.role
                  )}
                </Td>
                <Td align="right">
                  {canManage && (
                    <Button variant="danger" size="sm" onClick={() => kick(m.user_id)}>
                      Remove
                    </Button>
                  )}
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      </section>

      {canManage && (
        <section>
          <h3 className="font-medium mb-2">Pending invitations</h3>
          {invs.length === 0 ? (
            <div className="text-fg-muted text-sm">None.</div>
          ) : (
            <Table caption="Pending organization invitations">
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
                    <Td>{i.role}</Td>
                    <Td className="text-fg-muted">
                      {formatDateTime(i.expires_at)}
                    </Td>
                    <Td align="right">
                      <Button variant="danger" size="sm" onClick={() => cancel(i.id)}>
                        Cancel
                      </Button>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
          )}
        </section>
      )}
    </div>
  );
}

// OrgBilling is a read-only plan/quota summary for org members. Editing
// the caps is super-admin only (the platform admin console at
// /admin/orgs). The data comes from the org usage view.
function OrgBilling({ orgID }: { orgID: string }) {
  const usageQuery = useQuery({
    queryKey: ["org-usage", orgID],
    queryFn: () => getOrgUsage(orgID),
  });
  const usage = usageQuery.data ?? null;
  const err = usageQuery.error ? errorMessage(usageQuery.error) : null;

  if (err) {
    return (
      <InlineBanner tone="danger" layout="inline">
        {err}
      </InlineBanner>
    );
  }
  if (!usage) {
    return (
      <div className="text-sm text-fg-muted">
        <Spinner size="sm" label="Loading usage" />
      </div>
    );
  }

  const rows: Array<[string, string]> = [
    ["Monthly run quota", usage.monthly_run_quota ? String(usage.monthly_run_quota) : "unlimited"],
    ["Runs this month", String(usage.runs_this_month)],
    ["Monthly cost cap", usage.monthly_cost_cap_usd ? fmtUSD(usage.monthly_cost_cap_usd) : "unlimited"],
    ["Spend this month", fmtUSD(usage.cost_usd_this_month)],
    ["Memory quota", fmtBytes(usage.effective_memory_quota_bytes)],
    ["Memory used", fmtBytes(usage.memory_used_bytes)],
    ["Teams", String(usage.teams ?? 0)],
    ["Members", String(usage.members)],
  ];

  return (
    <div className="space-y-4">
      <p className="text-sm text-fg-muted">
        The monthly budget is shared across every team in this organization. Caps
        are managed by platform admins.
      </p>
      <div className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] divide-y divide-border-subtle overflow-hidden">
        {rows.map(([k, v]) => (
          <div key={k} className="flex items-center justify-between px-4 py-2 text-sm">
            <span className="text-fg-muted">{k}</span>
            <span className="font-medium">{v}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

// OrgTeams lists the org's teams (from the identity tree — no extra
// fetch for the member's own orgs) and lets an org admin create one.
// This is the real target of the OrgSwitcher's "create one" deep-link
// (/orgs/:id?tab=teams).
function OrgTeams({ orgID, canManage }: { orgID: string; canManage: boolean }) {
  const { orgs, reloadIdentity, selectTeam } = useAuth();
  const queryClient = useQueryClient();
  // Server-sourced for the same reason as the invitation dropdown above: a
  // list read out of the caller's tree is empty for a super-admin who is
  // not a member, and an empty list reads as "this org has no team".
  const teamsQuery = useQuery({
    queryKey: ["org-team-summaries", orgID],
    queryFn: () => listOrgTeamSummaries(orgID),
  });
  // The caller's own role still comes from the tree — it is caller-scoped
  // and the server's team row has no such field. Absent for someone who
  // holds no grant, which is the honest answer rather than a blank that
  // looks like a missing value.
  const myRole = useMemo(() => {
    const byID = new Map<string, string>();
    for (const t of orgs.find((o) => o.org_id === orgID)?.teams ?? []) {
      byID.set(t.team_id, t.role);
    }
    return byID;
  }, [orgs, orgID]);
  const teams = useMemo(
    () =>
      (teamsQuery.data ?? []).map((t) => ({
        team_id: t.id,
        team_name: t.name,
        team_slug: t.slug,
        personal: t.personal,
        status: t.status,
        role: myRole.get(t.id) ?? null,
      })),
    [teamsQuery.data, myRole],
  );
  const [draft, setDraft] = useState({ name: "", slug: "" });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const create = async () => {
    if (!draft.name.trim()) return;
    setBusy(true);
    setErr(null);
    try {
      const t = await createOrgTeam({
        name: draft.name.trim(),
        slug: draft.slug.trim() || undefined,
        org_id: orgID,
      });
      setDraft({ name: "", slug: "" });
      // The list is server-sourced now, so the tree reload alone no longer
      // refreshes it — and a super-admin creating a team in an org they do
      // not belong to never appears in that tree at all.
      await queryClient.invalidateQueries({ queryKey: ["org-team-summaries", orgID] });
      // The new team lands in the identity tree; switch to it so the
      // operator continues in the context they just created.
      await reloadIdentity();
      if (t?.id) await selectTeam(t.id);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <p className="text-sm text-fg-muted">
        Teams partition the organization's resources (repos, runs, boards,
        secrets). Most orgs need just one — the studio only surfaces team
        switching when there are several.
      </p>
      {err && <InlineBanner tone="danger">{err}</InlineBanner>}
      <Table caption="Teams of this organization">
        <THead>
          <Tr>
            <Th>Name</Th>
            <Th>Slug</Th>
            <Th>Your role</Th>
          </Tr>
        </THead>
        <TBody>
          {teams.length === 0 && (
            <Tr>
              <Td colSpan={3} className="text-fg-muted">
                No teams yet.
              </Td>
            </Tr>
          )}
          {teams.map((t) => (
            <Tr key={t.team_id}>
              <Td>
                {/* The drill-down this list was missing: an org's teams are
                    a way IN to them, not just an inventory. */}
                <Link
                  href={`/teams/${t.team_id}`}
                  className="text-accent-text hover:underline"
                >
                  {t.team_name}
                </Link>
                {t.personal && (
                  <span className="ml-2 text-xs text-fg-muted">personal</span>
                )}
                {t.status && t.status !== "active" && (
                  <span className="ml-2 text-xs text-warning-fg">{t.status}</span>
                )}
              </Td>
              <Td className="font-mono text-xs">{t.team_slug}</Td>
              <Td className={t.role ? "" : "text-fg-muted"}>{t.role ?? "no grant"}</Td>
            </Tr>
          ))}
        </TBody>
      </Table>
      {canManage && (
        <div className="flex flex-wrap items-end gap-2">
          <label className="flex flex-col gap-1 text-xs text-fg-muted">
            Team name
            <Input
              size="sm"
              value={draft.name}
              onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
              placeholder="e.g. Platform"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-fg-muted">
            Slug (optional)
            <Input
              size="sm"
              value={draft.slug}
              onChange={(e) => setDraft((d) => ({ ...d, slug: e.target.value }))}
              placeholder="platform"
            />
          </label>
          <Button size="sm" onClick={() => void create()} disabled={busy || !draft.name.trim()}>
            {busy ? <Spinner size="sm" /> : "Create team"}
          </Button>
        </div>
      )}
    </div>
  );
}
