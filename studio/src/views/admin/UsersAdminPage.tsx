import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useState } from "react";
import { ChevronLeftIcon, ChevronRightIcon } from "@radix-ui/react-icons";
import { InlineBanner } from "@/components/ui/InlineBanner";

import { useAuth } from "@/auth/AuthContext";
import { type UserStatus, type UserView } from "@/api/auth";
import { FeatureUnavailableError, listAdminUsers, updateAdminUser } from "@/api/admin";

import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";

const PAGE = 50;

export default function UsersAdminPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;

  const [users, setUsers] = useState<UserView[]>([]);
  const [offset, setOffset] = useState(0);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [confirm, setConfirm] = useState<{
    user: UserView;
    action: "disable" | "enable" | "grant" | "revoke" | "force_change";
  } | null>(null);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Users</span>,
    right: <span className="text-xs text-fg-muted">{users.length} user(s)</span>,
  });

  const refresh = useCallback(
    async (off: number) => {
      setBusy(true);
      setErr(null);
      try {
        const r = await listAdminUsers({ offset: off, limit: PAGE });
        setUsers(r.users ?? []);
        setOffset(r.offset);
        setUnavailable(false);
      } catch (e) {
        if (e instanceof FeatureUnavailableError) setUnavailable(true);
        else setErr(errorMessage(e));
      } finally {
        setBusy(false);
      }
    },
    [],
  );

  useEffect(() => {
    if (isSuper) void refresh(0);
  }, [isSuper, refresh]);

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  if (unavailable) {
    return (
      <div className="p-6">
        <EmptyState
          title="User console not enabled"
          message="The /api/admin/users endpoint isn't available on this server."
        />
      </div>
    );
  }

  const runAction = async () => {
    if (!confirm) return;
    const target = confirm.user;
    setBusy(true);
    try {
      switch (confirm.action) {
        case "disable":
          await updateAdminUser(target.id, { status: "disabled" });
          break;
        case "enable":
          await updateAdminUser(target.id, { status: "active" });
          break;
        case "force_change":
          await updateAdminUser(target.id, { status: "pending_password_change" });
          break;
        case "grant":
          await updateAdminUser(target.id, { is_super_admin: true });
          break;
        case "revoke":
          await updateAdminUser(target.id, { is_super_admin: false });
          break;
      }
      setConfirm(null);
      await refresh(offset);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const guardSelfDemote = (u: UserView): string | null => {
    if (u.id === me?.id) {
      return "You can't change your own super-admin status here. Ask another super-admin.";
    }
    return null;
  };

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-4">
        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}

        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
          <div className="overflow-x-auto"><table className="w-full text-sm">
            <thead className="text-left text-fg-muted border-b border-border-subtle">
              <tr>
                <th className="px-3 py-2 font-medium">Email</th>
                <th className="px-3 py-2 font-medium">Name</th>
                <th className="px-3 py-2 font-medium">Status</th>
                <th className="px-3 py-2 font-medium">Super-admin</th>
                <th className="px-3 py-2 font-medium text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id} className="border-b border-border-subtle last:border-0 align-top">
                  <td className="px-3 py-2">
                    <div>{u.email}</div>
                    <div className="text-caption text-fg-subtle font-mono">{u.id}</div>
                  </td>
                  <td className="px-3 py-2 text-fg-muted">{u.name ?? "—"}</td>
                  <td className="px-3 py-2">
                    <StatusPill status={u.status} />
                  </td>
                  <td className="px-3 py-2">
                    {u.is_super_admin ? (
                      <span className="text-warning-fg text-xs">yes</span>
                    ) : (
                      <span className="text-fg-muted text-xs">no</span>
                    )}
                  </td>
                  <td className="px-3 py-2 text-right space-x-1 whitespace-nowrap">
                    {u.status === "disabled" ? (
                      <Button size="sm" variant="ghost" onClick={() => setConfirm({ user: u, action: "enable" })}>
                        Re-enable
                      </Button>
                    ) : (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="text-danger"
                        onClick={() => setConfirm({ user: u, action: "disable" })}
                      >
                        Disable
                      </Button>
                    )}
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => setConfirm({ user: u, action: "force_change" })}
                    >
                      Force password change
                    </Button>
                    {u.is_super_admin ? (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="text-warning-fg"
                        disabled={guardSelfDemote(u) != null}
                        title={guardSelfDemote(u) ?? "Revoke super-admin"}
                        onClick={() => setConfirm({ user: u, action: "revoke" })}
                      >
                        Revoke super-admin
                      </Button>
                    ) : (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="text-warning-fg"
                        disabled={guardSelfDemote(u) != null}
                        title={guardSelfDemote(u) ?? "Grant super-admin"}
                        onClick={() => setConfirm({ user: u, action: "grant" })}
                      >
                        Grant super-admin
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
              {users.length === 0 && (
                <tr>
                  <td className="px-3 py-6 text-center text-fg-muted" colSpan={5}>
                    No users on this page.
                  </td>
                </tr>
              )}
            </tbody>
          </table></div>
        </section>

        <div className="flex justify-between items-center">
          <Button
            size="sm"
            variant="ghost"
            leadingIcon={<ChevronLeftIcon />}
            disabled={busy || offset === 0}
            onClick={() => void refresh(Math.max(0, offset - PAGE))}
          >
            Previous
          </Button>
          <div className="text-xs text-fg-muted">
            Page offset {offset}
          </div>
          <Button
            size="sm"
            variant="ghost"
            trailingIcon={<ChevronRightIcon />}
            disabled={busy || users.length < PAGE}
            onClick={() => void refresh(offset + PAGE)}
          >
            Next
          </Button>
        </div>
      </div>

      <ConfirmDialog
        open={confirm !== null}
        title={confirmTitle(confirm?.action)}
        message={confirmMessage(confirm)}
        confirmLabel={confirm?.action === "enable" ? "Re-enable" : "Confirm"}
        confirmVariant={
          confirm?.action === "disable" || confirm?.action === "revoke"
            ? "danger"
            : "default"
        }
        onConfirm={() => void runAction()}
        onCancel={() => setConfirm(null)}
      />
    </div>
  );
}

function StatusPill({ status }: { status: UserStatus }) {
  const variant: Record<UserStatus, BadgeVariant> = {
    active: "success",
    disabled: "danger",
    pending_password_change: "warning",
  };
  return <Badge variant={variant[status] ?? "neutral"}>{status}</Badge>;
}

function confirmTitle(a?: string): string {
  switch (a) {
    case "disable":
      return "Disable user?";
    case "enable":
      return "Re-enable user?";
    case "force_change":
      return "Force password change?";
    case "grant":
      return "Grant super-admin?";
    case "revoke":
      return "Revoke super-admin?";
  }
  return "Confirm action";
}

function confirmMessage(
  c: { user: UserView; action: string } | null,
): React.ReactNode {
  if (!c) return null;
  switch (c.action) {
    case "disable":
      return (
        <>
          The account will be disabled and every active session revoked. The user will fail
          to sign in until you re-enable the account.
        </>
      );
    case "enable":
      return "The account will be re-enabled. Existing tokens are not restored automatically.";
    case "force_change":
      return (
        <>
          Marks the account as <code>pending_password_change</code>. The next sign-in attempt
          will be redirected to the forced-rotation flow.
        </>
      );
    case "grant":
      return "Grants platform-wide super-admin privileges. This bypasses every team-level gate.";
    case "revoke":
      return "The user loses platform-wide privileges. Team-level roles are preserved.";
  }
  return null;
}
