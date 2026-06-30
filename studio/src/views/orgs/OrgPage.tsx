import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { useLocation, useParams, useSearch } from "wouter";

import { hasOrgRole, useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Tabs } from "@/components/ui/Tabs";
import { useConfirm } from "@/hooks/useConfirm";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";

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

type Tab = "members" | "sso" | "usage" | "audit" | "billing";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "members", label: "Members + invitations" },
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
  const [members, setMembers] = useState<OrgMemberView[]>([]);
  const [invs, setInvs] = useState<OrgInvitationView[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState({ email: "", role: "member" });
  const [issuedToken, setIssuedToken] = useState<string | null>(null);
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
      const r = await createOrgInvitation(orgID, draft);
      setIssuedToken(r.token);
      setDraft({ email: "", role: "member" });
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
            <Button variant="primary" type="submit" loading={busy}>
              Send invite
            </Button>
          </form>
          {issuedToken && (
            <div className="text-xs bg-surface-0 border border-border-subtle rounded p-3 font-mono break-all">
              Invitation token (copy + email this — it appears once):
              <br />
              {issuedToken}
              <div className="mt-2 flex gap-2 items-center font-sans">
                <CopyButton value={issuedToken} label="Copy" copiedLabel="Copied" />
                <Button variant="ghost" size="sm" onClick={() => setIssuedToken(null)}>
                  Done — hide
                </Button>
              </div>
            </div>
          )}
        </section>
      )}

      <section>
        <h3 className="font-medium mb-2">Members</h3>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-xs uppercase tracking-wider text-fg-muted text-left">
              <tr>
                <th className="px-2 py-1">Email</th>
                <th className="px-2 py-1">Name</th>
                <th className="px-2 py-1">Org role</th>
                <th className="px-2 py-1"></th>
              </tr>
            </thead>
            <tbody>
              {members.map((m) => (
                <tr key={m.user_id} className="border-t border-border-subtle">
                  <td className="px-2 py-2">{m.email ?? m.user_id}</td>
                  <td className="px-2 py-2">{m.name ?? "—"}</td>
                  <td className="px-2 py-2">
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
                  </td>
                  <td className="px-2 py-2 text-right">
                    {canManage && (
                      <Button variant="danger" size="sm" onClick={() => kick(m.user_id)}>
                        Remove
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      {canManage && (
        <section>
          <h3 className="font-medium mb-2">Pending invitations</h3>
          {invs.length === 0 ? (
            <div className="text-fg-muted text-sm">None.</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead className="text-xs uppercase tracking-wider text-fg-muted text-left">
                  <tr>
                    <th className="px-2 py-1">Email</th>
                    <th className="px-2 py-1">Role</th>
                    <th className="px-2 py-1">Expires</th>
                    <th className="px-2 py-1"></th>
                  </tr>
                </thead>
                <tbody>
                  {invs.map((i) => (
                    <tr key={i.id} className="border-t border-border-subtle">
                      <td className="px-2 py-2">{i.email}</td>
                      <td className="px-2 py-2">{i.role}</td>
                      <td className="px-2 py-2 text-fg-muted">
                        {new Date(i.expires_at).toLocaleString()}
                      </td>
                      <td className="px-2 py-2 text-right">
                        <Button variant="danger" size="sm" onClick={() => cancel(i.id)}>
                          Cancel
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
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
  if (!usage) return <div className="text-sm text-fg-muted">Loading…</div>;

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
