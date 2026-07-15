import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { Tabs } from "@/components/ui/Tabs";
import { useLocation, useParams, useSearch } from "wouter";
import { useConfirm } from "@/hooks/useConfirm";
import { useCanManageTeam } from "@/hooks/useCanManageTeam";
import { useAuth } from "@/auth/AuthContext";
import {
  type InvitationView,
  type TeamMemberView,
  createInvitation,
  deleteInvitation,
  listInvitations,
  listTeamMembers,
  removeMember,
  updateMemberRole,
} from "@/api/byok";
import ApiKeysPanel from "@/views/account/ApiKeys";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";

import AuditTab from "./tabs/AuditTab";
import MemoryTab from "./tabs/MemoryTab";

const ROLES = ["viewer", "member", "admin", "owner"] as const;

// SSO, Usage, members-roster and billing are ORG-level — they live on the Org
// settings page (/orgs/:id). The team page keeps the team's own administrative
// resources: who can access this team, its API keys, audit and memory. The
// integration surfaces (forges, webhooks, secrets, bot bindings, model
// providers) moved to their own top-level destination (/integrations).
type Tab = "members" | "api-keys" | "audit" | "memory";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "members", label: "Team access" },
  { id: "api-keys", label: "API keys" },
  { id: "audit", label: "Audit log" },
  { id: "memory", label: "Memory" },
];

export default function TeamPage() {
  const params = useParams<{ id: string }>();
  const teamID = params.id;
  const { teams, activeOrg, activeRole } = useAuth();
  const team = useMemo(() => teams.find((t) => t.team_id === teamID), [teams, teamID]);
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

  const canManage = useCanManageTeam();

  // Breadcrumb: show "Org / Team" only when the org name actually adds
  // information. For the personal/default org (where org_name == team_name) the
  // prefix is pure redundancy ("SocialGouv / SocialGouv/socialgouv"), so we
  // collapse it to just the team name and drop the noisy /slug micro-suffix.
  const showOrgCrumb = !!activeOrg && team != null && activeOrg.org_name !== team.team_name;
  useHeaderSlot({
    left: team ? (
      <span className="text-sm font-semibold">
        {showOrgCrumb && (
          <span className="text-fg-muted font-normal">{activeOrg!.org_name} / </span>
        )}
        {team.team_name}
      </span>
    ) : (
      <span className="text-sm font-semibold">Team not found</span>
    ),
    right: team ? (
      <span className="text-xs text-fg-muted">Your role: {activeRole ?? "—"}</span>
    ) : null,
  });

  if (!team) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">You are not a member of this team.</p>
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
          {tab === "members" && <Members teamID={team.team_id} canManage={canManage} />}
          {tab === "api-keys" && (
            <ApiKeysPanel team={{ id: team.team_id, name: team.team_name }} />
          )}
          {tab === "audit" && <AuditTab teamID={team.team_id} canManage={canManage} />}
          {tab === "memory" && <MemoryTab teamID={team.team_id} />}
        </main>
      </div>
    </div>
  );
}

function Members({ teamID, canManage }: { teamID: string; canManage: boolean }) {
  const [members, setMembers] = useState<TeamMemberView[]>([]);
  const [invs, setInvs] = useState<InvitationView[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [draft, setDraft] = useState({ email: "", role: "member" });
  const [issuedToken, setIssuedToken] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();

  const reload = async () => {
    setErr(null);
    try {
      const [m, i] = await Promise.all([listTeamMembers(teamID), listInvitations(teamID)]);
      setMembers(m);
      setInvs(i);
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teamID]);

  const invite = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      const r = await createInvitation(teamID, draft);
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
      await deleteInvitation(teamID, id);
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  const setRole = async (userID: string, currentRole: string, role: string) => {
    if (role === currentRole) return;
    // Confirm demotions and any change touching "owner" — these are the
    // role edits that can lock someone out or hand over control. Routine
    // promotions (e.g. member → admin) apply without a prompt.
    const demotion =
      ROLES.indexOf(role as (typeof ROLES)[number]) <
      ROLES.indexOf(currentRole as (typeof ROLES)[number]);
    if (demotion || currentRole === "owner" || role === "owner") {
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
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
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
                <CopyButton
                  value={issuedToken}
                  label="Copy"
                  copiedLabel="Copied"
                />
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setIssuedToken(null)}
                >
                  Done — hide
                </Button>
              </div>
            </div>
          )}
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
                  <Td>{i.role}</Td>
                  <Td className="text-fg-muted">
                    {new Date(i.expires_at).toLocaleString()}
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
