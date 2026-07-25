import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronLeftIcon, ChevronRightIcon } from "@radix-ui/react-icons";
import { InlineBanner } from "@/components/ui/InlineBanner";

import { useAuth } from "@/auth/AuthContext";
import { type UserStatus, type UserView } from "@/api/auth";
import {
  FeatureUnavailableError,
  listAdminUsers,
  resetAdminUserPassword,
  updateAdminUser,
} from "@/api/admin";

import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { EmptyState } from "@/components/ui/EmptyState";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";

import AdminNav from "./AdminNav";

const PAGE = 50;

export default function UsersAdminPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;
  // User administration is a cloud-mode console (/api/admin/users isn't
  // registered locally) — gate on server_info BEFORE fetching so local
  // mode never fires a doomed 404 request.
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  const queryClient = useQueryClient();
  // The offset requested by the pagination buttons; the query refetches on
  // change. keepPreviousData holds the previous page's rows on screen while
  // the next one loads — the manual fetch replaced them only on response.
  const [offset, setOffset] = useState(0);
  const query = useQuery({
    queryKey: ["admin-users", offset],
    queryFn: () => listAdminUsers({ offset, limit: PAGE }),
    enabled: isSuper && isCloud,
    placeholderData: keepPreviousData,
  });
  const users = query.data?.users ?? [];
  // Server-echoed offset — while a page is in flight this stays on the
  // previous page's value, like the manual fetch did.
  const effOffset = query.data?.offset ?? offset;
  // Drives the initial TableSkeleton only — pagination/refetch keep the
  // table on screen (isPending stays false once any page exists).
  const loaded = !query.isPending;
  const unavailable = query.error instanceof FeatureUnavailableError;
  // Mutation failures share the banner with the fetch error (mutation wins,
  // like the old single slot); the fetch error hides while a page load is
  // in flight, which the manual refresh achieved by clearing it up front.
  const [mutErr, setMutErr] = useState<string | null>(null);
  const err =
    mutErr ??
    (query.error && !unavailable && !query.isFetching
      ? errorMessage(query.error)
      : null);
  const [mutBusy, setMutBusy] = useState(false);
  const busy = mutBusy || query.isFetching;
  const [confirm, setConfirm] = useState<{
    user: UserView;
    action: "disable" | "enable" | "grant" | "revoke" | "force_change" | "reset_password";
  } | null>(null);
  // One-shot temp password issued by reset_password — shown until
  // dismissed; the server never returns it again.
  const [tempIssued, setTempIssued] = useState<{
    email: string;
    password: string;
  } | null>(null);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Users</span>,
    right: <span className="text-xs text-fg-muted">{users.length} user(s)</span>,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  // Deliberate local-mode gate: no fetch fired. While server_info is still
  // loading we fall through to the skeleton below instead of flashing this
  // notice on cloud.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="User administration" />
        </div>
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
    setMutBusy(true);
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
        case "reset_password": {
          const res = await resetAdminUserPassword(target.id);
          setTempIssued({ email: target.email, password: res.temp_password });
          break;
        }
      }
      setConfirm(null);
      setMutErr(null);
      // Refetch the current page (and mark the others stale).
      await queryClient.invalidateQueries({ queryKey: ["admin-users"] });
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setMutBusy(false);
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
        <AdminNav />

        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}

        {tempIssued && (
          <InlineBanner
            tone="warning"
            layout="inline"
            title={`Temporary password for ${tempIssued.email} — it appears once`}
            dismissable
            onDismiss={() => setTempIssued(null)}
          >
            <div className="flex items-center gap-2">
              <code className="font-mono text-micro break-all">
                {tempIssued.password}
              </code>
              <CopyButton value={tempIssued.password} label="Copy temp password" />
            </div>
            <p className="mt-1">
              Hand it to the user out-of-band. Their next sign-in forces
              them to choose a new password (the temporary one is the
              &quot;current password&quot; of that step).
            </p>
          </InlineBanner>
        )}

        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
          {!loaded ? (
            <div className="p-3">
              <TableSkeleton rows={5} cols={5} />
            </div>
          ) : users.length === 0 ? (
            <EmptyState message="No users on this page." />
          ) : (
          <Table caption="Platform users">
            <THead>
              <Th>Email</Th>
              <Th>Name</Th>
              <Th>Status</Th>
              <Th>Super-admin</Th>
              <Th align="right">Actions</Th>
            </THead>
            <TBody>
              {users.map((u) => (
                <Tr key={u.id} className="align-top">
                  <Td>
                    <div>{u.email}</div>
                    <div className="text-caption text-fg-subtle font-mono">{u.id}</div>
                  </Td>
                  <Td className="text-fg-muted">{u.name ?? "—"}</Td>
                  <Td>
                    <StatusPill status={u.status} />
                  </Td>
                  <Td>
                    {u.is_super_admin ? (
                      <span className="text-warning-fg text-xs">yes</span>
                    ) : (
                      <span className="text-fg-muted text-xs">no</span>
                    )}
                  </Td>
                  <Td align="right" className="space-x-1 whitespace-nowrap">
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
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => setConfirm({ user: u, action: "reset_password" })}
                    >
                      Reset password
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
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
          )}
        </section>

        <div className="flex justify-between items-center">
          <Button
            size="sm"
            variant="ghost"
            leadingIcon={<ChevronLeftIcon />}
            disabled={busy || effOffset === 0}
            onClick={() => setOffset(Math.max(0, effOffset - PAGE))}
          >
            Previous
          </Button>
          <div className="text-xs text-fg-muted">
            Page offset {effOffset}
          </div>
          <Button
            size="sm"
            variant="ghost"
            trailingIcon={<ChevronRightIcon />}
            disabled={busy || users.length < PAGE}
            onClick={() => setOffset(effOffset + PAGE)}
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
    case "reset_password":
      return "Reset password?";
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
          is redirected to the forced-rotation flow — where the user must still enter their
          CURRENT password. Use this to require a rotation, not to recover a lost password
          (that&apos;s &quot;Reset password&quot;).
        </>
      );
    case "reset_password":
      return (
        <>
          Replaces the user&apos;s password with a one-shot temporary one (shown to you once)
          and revokes every active session. Use this to recover a locked-out account; the
          user picks a new password at their next sign-in.
        </>
      );
    case "grant":
      return "Grants platform-wide super-admin privileges. This bypasses every team-level gate.";
    case "revoke":
      return "The user loses platform-wide privileges. Team-level roles are preserved.";
  }
  return null;
}
