import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "wouter";
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
import ApiKeysPanel from "@/views/settings/ApiKeys";

const ROLES = ["viewer", "member", "admin", "owner"] as const;

type Tab = "members" | "api-keys";

export default function TeamPage() {
  const params = useParams<{ id: string }>();
  const teamID = params.id;
  const { teams, activeRole, user } = useAuth();
  const team = useMemo(() => teams.find((t) => t.team_id === teamID), [teams, teamID]);
  const [tab, setTab] = useState<Tab>("members");

  if (!team) {
    return (
      <div className="min-h-screen bg-surface-0 text-fg-default p-6">
        <Link href="/" className="text-fg-muted hover:underline">
          ← Editor
        </Link>
        <h1 className="text-lg font-semibold mt-4">Team not found</h1>
        <p className="text-sm text-fg-muted">You are not a member of this team.</p>
      </div>
    );
  }
  const canManage =
    activeRole === "admin" || activeRole === "owner" || (user?.is_super_admin ?? false);

  return (
    <div className="min-h-screen bg-surface-0 text-fg-default">
      <header className="bg-surface-1 border-b border-border-subtle px-6 py-3 flex items-center justify-between">
        <div className="flex items-center gap-4">
          <Link href="/" className="text-fg-muted hover:underline">
            ← Editor
          </Link>
          <h1 className="text-lg font-semibold">
            {team.team_name}
            <span className="ml-2 text-sm text-fg-muted">/{team.team_slug}</span>
          </h1>
        </div>
        <div className="text-sm text-fg-muted">Your role: {activeRole ?? "—"}</div>
      </header>

      <div className="max-w-5xl mx-auto p-6 grid grid-cols-[200px,1fr] gap-6">
        <nav className="space-y-1">
          {(
            [
              { id: "members", label: "Members + invitations" },
              { id: "api-keys", label: "API keys" },
            ] as Array<{ id: Tab; label: string }>
          ).map((t) => (
            <button
              key={t.id}
              onClick={() => setTab(t.id)}
              className={`w-full text-left px-3 py-2 rounded text-sm ${
                tab === t.id ? "bg-surface-2" : "hover:bg-surface-1"
              }`}
            >
              {t.label}
            </button>
          ))}
        </nav>

        <main>
          {tab === "members" && <Members teamID={team.team_id} canManage={canManage} />}
          {tab === "api-keys" && (
            <ApiKeysPanel team={{ id: team.team_id, name: team.team_name }} />
          )}
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

  const reload = async () => {
    setErr(null);
    try {
      const [m, i] = await Promise.all([listTeamMembers(teamID), listInvitations(teamID)]);
      setMembers(m);
      setInvs(i);
    } catch (e) {
      setErr((e as Error).message);
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
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const cancel = async (id: string) => {
    if (!confirm("Cancel this invitation?")) return;
    try {
      await deleteInvitation(teamID, id);
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  const setRole = async (userID: string, role: string) => {
    try {
      await updateMemberRole(teamID, userID, role);
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  const kick = async (userID: string) => {
    if (!confirm("Remove this member from the team?")) return;
    try {
      await removeMember(teamID, userID);
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  return (
    <div className="space-y-6">
      {err && (
        <div className="text-sm text-fg-error bg-surface-warn-subtle border border-border-warn rounded px-3 py-2">
          {err}
        </div>
      )}

      {canManage && (
        <section className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
          <h3 className="font-medium">Invite a member</h3>
          <form onSubmit={invite} className="flex gap-2">
            <input
              type="email"
              className="flex-1 bg-surface-0 border border-border-subtle rounded px-3 py-2"
              placeholder="email@example.com"
              value={draft.email}
              onChange={(e) => setDraft({ ...draft, email: e.target.value })}
              required
            />
            <select
              className="bg-surface-0 border border-border-subtle rounded px-3 py-2"
              value={draft.role}
              onChange={(e) => setDraft({ ...draft, role: e.target.value })}
            >
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
            <button
              type="submit"
              disabled={busy}
              className="bg-fg-accent text-surface-0 rounded px-3 py-2 text-sm disabled:opacity-50"
            >
              Send invite
            </button>
          </form>
          {issuedToken && (
            <div className="text-xs bg-surface-0 border border-border-subtle rounded p-3 font-mono break-all">
              Invitation token (copy + email this — it appears once):
              <br />
              {issuedToken}
            </div>
          )}
        </section>
      )}

      <section>
        <h3 className="font-medium mb-2">Members</h3>
        <table className="w-full text-sm">
          <thead className="text-xs uppercase tracking-wider text-fg-muted text-left">
            <tr>
              <th className="px-2 py-1">Email</th>
              <th className="px-2 py-1">Name</th>
              <th className="px-2 py-1">Role</th>
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
                    <select
                      value={m.role}
                      onChange={(e) => setRole(m.user_id, e.target.value)}
                      className="bg-surface-0 border border-border-subtle rounded px-2 py-1 text-xs"
                    >
                      {ROLES.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </select>
                  ) : (
                    m.role
                  )}
                </td>
                <td className="px-2 py-2 text-right">
                  {canManage && (
                    <button onClick={() => kick(m.user_id)} className="text-fg-error hover:underline text-xs">
                      Remove
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <section>
        <h3 className="font-medium mb-2">Pending invitations</h3>
        {invs.length === 0 ? (
          <div className="text-fg-muted text-sm">None.</div>
        ) : (
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
                    {canManage && (
                      <button onClick={() => cancel(i.id)} className="text-fg-error hover:underline text-xs">
                        Cancel
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}
