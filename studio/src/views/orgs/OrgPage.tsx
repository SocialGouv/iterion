import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { useLocation, useParams, useSearch } from "wouter";

import { hasOrgRole, useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Spinner } from "@/components/ui/Spinner";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { Tabs } from "@/components/ui/Tabs";
import { useConfirm } from "@/hooks/useConfirm";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import InviteLinkPanel from "@/components/shared/InviteLinkPanel";


import {
  type OrgInvitationView,
  type OrgMemberView,
  createOrgInvitation,
  deleteOrgInvitation,
  listOrgInvitations,
  listOrgMembers,
  removeOrgMember,
  updateOrgMemberRole,
} from "@/api/orgMembers";
import { type OrgRole } from "@/api/auth";
import { createOrgTeam } from "@/api/orgs";
import {
  type OrgUsage,
  fmtBytes,
  fmtUSD,
  getOrgUsage,
} from "@/api/usage";
import SSOTab from "@/views/teams/tabs/SSOTab";
import UsageTab from "@/views/teams/tabs/UsageTab";
import AuditTab from "@/views/teams/tabs/AuditTab";

const ORG_ROLES: OrgRole[] = ["member", "admin", "owner"];

type Tab = "members" | "teams" | "sso" | "usage" | "audit" | "billing";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "members", label: "Members + invitations" },
  { id: "teams", label: "Teams" },
  { id: "sso", label: "SSO" },
  { id: "usage", label: "Usage" },
  { id: "audit", label: "Audit log" },
  { id: "billing", label: "Plan + quotas" },
];

export default function OrgPage() {
  const params = useParams<{ id: string }>();
  const orgID = params.id;
  const { orgs, activeOrgRole, user } = useAuth();
  const org = useMemo(() => orgs.find((o) => o.org_id === orgID), [orgs, orgID]);
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

  // canManage uses the active org role; an org admin/owner (or super-admin)
  // can mutate. When inspecting an org that isn't the active one we fall
  // back to the role recorded in the tree.
  const orgRole = (org?.org_role || activeOrgRole || null) as OrgRole | null;
  const canManage = !!user?.is_super_admin || hasOrgRole(orgRole, "admin");

  useHeaderSlot({
    left: org ? (
      <span className="text-sm font-semibold">
        {org.org_name}
        <span className="ml-2 text-xs text-fg-muted font-normal">/{org.org_slug}</span>
      </span>
    ) : (
      <span className="text-sm font-semibold">Organization not found</span>
    ),
    right: org ? (
      <span className="text-xs text-fg-muted">Your org role: {orgRole ?? "—"}</span>
    ) : null,
  });

  if (!org) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">You are not a member of this organization.</p>
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
          {tab === "members" && <OrgMembers orgID={org.org_id} canManage={canManage} />}
          {tab === "teams" && <OrgTeams orgID={org.org_id} canManage={canManage} />}
          {tab === "sso" && <SSOTab teamID={org.org_id} canManage={canManage} />}
          {tab === "usage" && <UsageTab orgID={org.org_id} />}
          {tab === "audit" && <AuditTab orgID={org.org_id} canManage={canManage} />}
          {tab === "billing" && <OrgBilling orgID={org.org_id} />}
        </main>
      </div>
    </div>
  );
}

function OrgMembers({ orgID, canManage }: { orgID: string; canManage: boolean }) {
  const { orgs } = useAuth();
  const orgTeams = useMemo(
    () => orgs.find((o) => o.org_id === orgID)?.teams ?? [],
    [orgs, orgID],
  );
  const [members, setMembers] = useState<OrgMemberView[]>([]);
  const [invs, setInvs] = useState<OrgInvitationView[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState({ email: "", role: "member", team_id: "" });
  // Every invite issued in this session, newest first — tokens appear
  // once server-side, so a second invite must not clobber the first's
  // still-uncopied link.
  const [issued, setIssued] = useState<Array<{ email: string; token: string }>>([]);
  const { confirm, dialog } = useConfirm();

  const reload = async () => {
    setErr(null);
    try {
      const [m, i] = await Promise.all([
        listOrgMembers(orgID),
        canManage ? listOrgInvitations(orgID) : Promise.resolve<OrgInvitationView[]>([]),
      ]);
      setMembers(m);
      setInvs(i);
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgID, canManage]);

  const invite = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      const r = await createOrgInvitation(orgID, {
        email: draft.email,
        role: draft.role,
        team_id: draft.team_id || undefined,
      });
      setIssued((list) => [{ email: draft.email, token: r.token }, ...list]);
      setDraft({ email: "", role: "member", team_id: draft.team_id });
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
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
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
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
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
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
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
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
                      {new Date(i.expires_at).toLocaleString()}
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
  const [usage, setUsage] = useState<OrgUsage | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    getOrgUsage(orgID)
      .then((u) => alive && setUsage(u))
      .catch((e) => alive && setErr(errorMessage(e)));
    return () => {
      alive = false;
    };
  }, [orgID]);

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
  const org = useMemo(() => orgs.find((o) => o.org_id === orgID), [orgs, orgID]);
  const teams = org?.teams ?? [];
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
                {t.team_name}
                {t.personal && (
                  <span className="ml-2 text-xs text-fg-muted">personal</span>
                )}
              </Td>
              <Td className="font-mono text-xs">{t.team_slug}</Td>
              <Td>{t.role}</Td>
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
