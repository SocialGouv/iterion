import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { clickableRowProps } from "@/lib/a11y";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useAuth } from "@/auth/AuthContext";
import {
  type OrgView,
  createOrg,
  deleteOrg,
  fmtQuotaGiB,
  gibToBytes,
  listOrgs,
  restoreOrg,
  setOrgStatus,
  updateOrg,
} from "@/api/orgs";
import { FeatureUnavailableError, getAdminOrgUsage, type OrgUsage, fmtBytes, fmtUSD, pct } from "@/api/usage";

import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";

export default function OrgsAdminPage() {
  const { user, activeOrgID, reloadIdentity } = useAuth();
  const isSuper = user?.is_super_admin ?? false;
  // Org administration is a cloud-mode console (/api/admin/orgs isn't
  // registered locally) — gate on server_info BEFORE fetching so local
  // mode never fires a doomed 404 and never shows an enabled create form.
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  const [orgs, setOrgs] = useState<OrgView[]>([]);
  // loaded flips once the first list fetch settles — drives the initial
  // TableSkeleton without re-skeletoning on post-mutation refreshes.
  const [loaded, setLoaded] = useState(false);
  const [name, setName] = useState("");
  const [ownerEmail, setOwnerEmail] = useState("");
  const [active, setActive] = useState<OrgView | null>(null);
  // Single useAsyncAction underpins the refresh + every mutation
  // (create, update, status). The shared error slot maps the
  // FeatureUnavailable exception to a friendlier line; everything else
  // falls through to the hook's default errorMessage().
  const action = useAsyncAction();
  const { busy, error, run: actionRun, setError } = action;

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Organizations</span>,
    right: <span className="text-xs text-fg-muted">{orgs.length} org(s)</span>,
  });

  const refresh = useCallback(async () => {
    try {
      setOrgs(await listOrgs());
      setError(null);
    } catch (e) {
      setError(
        e instanceof FeatureUnavailableError
          ? "Organization administration is a cloud-mode feature — it isn't available on this server (local/desktop mode)."
          : String(e),
      );
    } finally {
      setLoaded(true);
    }
  }, [setError]);

  useEffect(() => {
    if (isSuper && isCloud) void refresh();
  }, [isSuper, isCloud, refresh]);

  // Wrap a mutation in the shared busy/error slot, then refresh the
  // list once it settles successfully — identical sequencing to the
  // pre-extract handler. useAsyncAction maps the thrown error to its
  // hook's errorMessage(); since this view treats FeatureUnavailable
  // specially via refresh()'s own catch, that path is the only
  // remaining bespoke branch.
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      const ok = await actionRun(async () => {
        await fn();
        return true;
      });
      if (ok) await refresh();
    },
    [actionRun, refresh],
  );

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  // Deliberate local-mode gate: no fetch fired, no enabled create form.
  // While server_info is still loading we fall through to the skeleton
  // below instead of flashing this notice on cloud.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Organization administration" />
        </div>
      </div>
    );
  }

  const create = (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) return;
    void run(async () => {
      await createOrg({ name: name.trim(), owner_email: ownerEmail.trim() || undefined });
      setName("");
      setOwnerEmail("");
      // Refresh the AuthContext org tree (it feeds the org switcher in
      // UserTeamChip), not just this page's local table — otherwise the new
      // org only appears after a full page reload.
      await reloadIdentity();
    });
  };

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-4">
        {error && (
          <InlineBanner tone="danger" layout="inline">
            {error}
          </InlineBanner>
        )}

        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-3">
          <h3 className="font-medium">Create an organization</h3>
          <form onSubmit={create} className="flex flex-wrap gap-2 items-start">
            <div className="flex-1 min-w-[160px]">
              <label htmlFor="create-org-name" className="sr-only">
                Org name
              </label>
              <Input
                size="md"
                id="create-org-name"
                placeholder="Org name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </div>
            <div className="flex-1 min-w-[200px]">
              <label htmlFor="create-org-owner" className="sr-only">
                Owner email
              </label>
              <Input
                size="md"
                type="email"
                id="create-org-owner"
                placeholder="owner email (optional)"
                value={ownerEmail}
                onChange={(e) => setOwnerEmail(e.target.value)}
              />
            </div>
            <Button variant="primary" type="submit" loading={busy}>
              Create
            </Button>
          </form>
        </section>

        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
          {!loaded ? (
            <div className="p-3">
              <TableSkeleton rows={4} cols={5} />
            </div>
          ) : orgs.length === 0 ? (
            <EmptyState message="No organizations yet." />
          ) : (
            <Table caption="Organizations">
              <THead>
                <Th>Name</Th>
                <Th>Slug</Th>
                <Th>Status</Th>
                <Th>Memory quota</Th>
                <Th align="right">Manage</Th>
              </THead>
              <TBody>
                {orgs.map((o) => {
                  const statusVariant: BadgeVariant =
                    o.status === "suspended" || o.status === "pending_deletion"
                      ? "danger"
                      : o.status === "read_only"
                        ? "neutral"
                        : "success";
                  return (
                    <Tr
                      key={o.id}
                      className="cursor-pointer"
                      {...clickableRowProps(() => setActive(o), `Open ${o.name} (${o.status})`)}
                    >
                      <Td>
                        {o.name}
                        {o.personal && <span className="ml-2 text-xs text-fg-muted">personal</span>}
                      </Td>
                      <Td className="text-fg-muted">{o.slug}</Td>
                      <Td>
                        <Badge variant={statusVariant}>{o.status}</Badge>
                      </Td>
                      <Td className="text-fg-muted">{fmtQuotaGiB(o.memory_quota_bytes)}</Td>
                      <Td align="right">
                        <Button size="sm" variant="ghost" onClick={(e) => { e.stopPropagation(); setActive(o); }}>
                          Open
                        </Button>
                      </Td>
                    </Tr>
                  );
                })}
              </TBody>
            </Table>
          )}
        </section>
      </div>

      {active && (
        <OrgDrawer
          org={active}
          busy={busy}
          isActiveOrg={active.id === activeOrgID}
          onClose={() => setActive(null)}
          onChanged={setActive}
          onAfterUpdate={refresh}
          reloadIdentity={reloadIdentity}
          run={run}
        />
      )}
    </div>
  );
}

function OrgDrawer({
  org,
  busy,
  isActiveOrg,
  onClose,
  onChanged,
  onAfterUpdate,
  reloadIdentity,
  run,
}: {
  org: OrgView;
  busy: boolean;
  isActiveOrg: boolean;
  onClose: () => void;
  onChanged: (o: OrgView) => void;
  onAfterUpdate: () => Promise<void>;
  reloadIdentity: () => Promise<void>;
  run: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const [usage, setUsage] = useState<OrgUsage | null>(null);
  const [usageErr, setUsageErr] = useState<string | null>(null);

  // Name + slug drafts.
  const [nameDraft, setNameDraft] = useState(org.name);
  const [slugDraft, setSlugDraft] = useState(org.slug);

  // Quota draft state — initialised from org.
  const initialGiB = org.memory_quota_bytes ? org.memory_quota_bytes / (1 << 30) : 0;
  const [memGiB, setMemGiB] = useState<number>(initialGiB);
  const [monthlyRuns, setMonthlyRuns] = useState<number>(org.monthly_run_quota ?? 0);
  const [costCap, setCostCap] = useState<number>(org.monthly_cost_cap_usd ?? 0);

  // Status draft.
  const [statusDraft, setStatusDraft] = useState<string>(org.status);
  const [reason, setReason] = useState("");
  // Two-step confirm for the disruptive (suspend / read_only) statuses.
  // Inline (not useConfirm) because this lives inside a Radix Dialog,
  // where a body-portaled ConfirmDialog reads as an outside-click and
  // dismisses the parent — see ProjectSwitcher for the same precedent.
  const [confirmStatus, setConfirmStatus] = useState(false);
  // Typed confirmation for the irreversible org deletion.
  const [confirmName, setConfirmName] = useState("");

  useEffect(() => {
    let alive = true;
    setUsageErr(null);
    getAdminOrgUsage(org.id)
      .then((u) => {
        if (alive) setUsage(u);
      })
      .catch((e) => {
        if (!alive) return;
        if (e instanceof FeatureUnavailableError) setUsageErr("Usage view not enabled.");
        else setUsageErr(errorMessage(e));
      });
    return () => {
      alive = false;
    };
  }, [org.id]);

  const saveDetails = () =>
    run(async () => {
      await updateOrg(org.id, { name: nameDraft.trim(), slug: slugDraft.trim() });
      await onAfterUpdate();
      // Reflect the new name/slug in the org switcher + breadcrumbs immediately.
      await reloadIdentity();
    });

  const saveQuotas = () =>
    run(async () => {
      await updateOrg(org.id, {
        memory_quota_bytes: memGiB > 0 ? gibToBytes(memGiB) : 0,
        monthly_run_quota: monthlyRuns,
        monthly_cost_cap_usd: costCap,
      });
      await onAfterUpdate();
    });

  const saveStatus = () =>
    run(async () => {
      await setOrgStatus(org.id, statusDraft, reason.trim() || undefined);
      await onAfterUpdate();
    });

  // Soft-delete: schedules purge after a 24h grace; the drawer stays open and
  // flips to the "scheduled — cancel" state. reloadIdentity drops the now-blocked
  // org from the switcher.
  const scheduleDelete = () =>
    run(async () => {
      const updated = await deleteOrg(org.id);
      await onAfterUpdate();
      await reloadIdentity();
      setConfirmName("");
      onChanged(updated);
    });

  const cancelDelete = () =>
    run(async () => {
      const updated = await restoreOrg(org.id);
      await onAfterUpdate();
      await reloadIdentity();
      onChanged(updated);
    });

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={org.name}
      description={
        <span>
          <span className="font-mono text-xs">{org.id}</span>
          {org.personal ? " · personal" : ""}
        </span>
      }
      widthClass="max-w-3xl"
      footer={
        <Button variant="secondary" onClick={onClose}>
          Close
        </Button>
      }
    >
      {usageErr && (
        <div className="text-sm text-fg-muted bg-warning-soft border border-warning/40 rounded px-3 py-2 mb-3">
          {usageErr}
        </div>
      )}

      <section className="space-y-3 mb-4">
        <h4 className="font-medium">Details</h4>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
          <Field label="Name">
            <Input value={nameDraft} onChange={(e) => setNameDraft(e.target.value)} />
          </Field>
          <Field label="Slug (URL identifier)">
            <Input
              value={slugDraft}
              onChange={(e) => setSlugDraft(e.target.value)}
              placeholder="my-org"
            />
          </Field>
        </div>
        <Button
          variant="primary"
          loading={busy}
          disabled={
            nameDraft.trim() === "" ||
            (nameDraft.trim() === org.name && slugDraft.trim() === org.slug)
          }
          onClick={() => void saveDetails()}
        >
          Save details
        </Button>
      </section>

      <section className="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs mb-4">
        <Stat title="Members" value={String(usage?.members ?? "—")} />
        <Stat
          title="Memory"
          value={
            usage
              ? `${fmtBytes(usage.memory_used_bytes)} / ${fmtBytes(usage.effective_memory_quota_bytes)}`
              : "—"
          }
          progress={usage ? pct(usage.memory_used_bytes, usage.effective_memory_quota_bytes) : null}
        />
        <Stat
          title="Runs this month"
          value={
            usage
              ? `${usage.runs_this_month}${usage.monthly_run_quota > 0 ? ` / ${usage.monthly_run_quota}` : ""}`
              : "—"
          }
          progress={usage ? pct(usage.runs_this_month, usage.monthly_run_quota) : null}
        />
        <Stat
          title="Cost this month"
          value={
            usage
              ? `${fmtUSD(usage.cost_usd_this_month)}${
                  usage.monthly_cost_cap_usd && usage.monthly_cost_cap_usd > 0
                    ? ` / ${fmtUSD(usage.monthly_cost_cap_usd)}`
                    : ""
                }`
              : "—"
          }
          progress={
            usage && usage.monthly_cost_cap_usd
              ? pct(usage.cost_usd_this_month, usage.monthly_cost_cap_usd)
              : null
          }
        />
        <Stat title="API keys" value={String(usage?.api_key_count ?? "—")} />
        <Stat title="Secrets" value={String(usage?.generic_secret_count ?? "—")} />
        <Stat title="Bindings" value={String(usage?.bot_binding_count ?? "—")} />
        <Stat title="Webhooks" value={String(usage?.webhook_count ?? "—")} />
      </section>

      <section className="space-y-3 mb-6">
        <h4 className="font-medium">Quotas</h4>
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-2">
          <Field label="Memory quota (GiB, 0 = default)">
            <Input
              type="number"
              min={0}
              step={0.5}
              value={String(memGiB)}
              onChange={(e) => setMemGiB(Number(e.target.value))}
            />
          </Field>
          <Field label="Monthly run quota (0 = unlimited)">
            <Input
              type="number"
              min={0}
              value={String(monthlyRuns)}
              onChange={(e) => setMonthlyRuns(Number(e.target.value))}
            />
          </Field>
          <Field label="Monthly cost cap USD (0 = unlimited)">
            <Input
              type="number"
              min={0}
              step={1}
              value={String(costCap)}
              onChange={(e) => setCostCap(Number(e.target.value))}
            />
          </Field>
        </div>
        <Button variant="primary" loading={busy} onClick={() => void saveQuotas()}>
          Save quotas
        </Button>
      </section>

      <section className="space-y-3">
        <h4 className="font-medium">Status</h4>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
          <Field label="Status">
            <Select
              value={statusDraft}
              onChange={(e) => {
                setStatusDraft(e.target.value);
                setConfirmStatus(false);
              }}
            >
              <option value="active">active</option>
              <option value="suspended">suspended</option>
              <option value="read_only">read_only</option>
            </Select>
          </Field>
          <Field label="Reason (audit log)">
            <Input
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="optional"
            />
          </Field>
        </div>
        {(() => {
          const disruptive =
            statusDraft === "suspended" || statusDraft === "read_only";
          if (disruptive && confirmStatus) {
            return (
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-xs text-danger">
                  Applies <strong>{statusDraft}</strong> to the whole org immediately.
                </span>
                <Button
                  variant="ghost"
                  onClick={() => setConfirmStatus(false)}
                  disabled={busy}
                >
                  Cancel
                </Button>
                <Button
                  variant="danger"
                  loading={busy}
                  onClick={() => {
                    setConfirmStatus(false);
                    void saveStatus();
                  }}
                >
                  Confirm — apply {statusDraft}
                </Button>
              </div>
            );
          }
          return (
            <Button
              variant={disruptive ? "danger" : "primary"}
              loading={busy}
              onClick={() => {
                if (disruptive) {
                  setConfirmStatus(true);
                  return;
                }
                void saveStatus();
              }}
            >
              Apply status
            </Button>
          );
        })()}
      </section>

      <section className="space-y-3 mt-6 border-t border-danger/30 pt-4">
        <h4 className="font-medium text-danger">Danger zone</h4>
        {org.status === "pending_deletion" ? (
          <>
            <p className="text-xs text-fg-muted">
              Deletion scheduled
              {org.purge_after
                ? ` — will be permanently purged after ${new Date(org.purge_after).toLocaleString()}`
                : ""}
              . The organization is blocked until then. You can still cancel.
            </p>
            <Button variant="secondary" loading={busy} onClick={() => void cancelDelete()}>
              Cancel deletion
            </Button>
          </>
        ) : isActiveOrg ? (
          <p className="text-xs text-fg-muted">
            This is your active organization — switch to another org (top-left
            switcher) before you can delete it.
          </p>
        ) : (
          <>
            <p className="text-xs text-fg-muted">
              Schedules permanent deletion of <strong>{org.name}</strong>. The org is
              blocked immediately, then after a 24h grace a nightly job purges it,
              its teams, every membership, and all team data (runs, board, forge,
              secrets…). Cancellable until the grace elapses.
            </p>
            <Field label={`Type "${org.name}" to confirm`}>
              <Input
                value={confirmName}
                onChange={(e) => setConfirmName(e.target.value)}
                placeholder={org.name}
              />
            </Field>
            <Button
              variant="danger"
              loading={busy}
              disabled={confirmName.trim() !== org.name}
              onClick={() => void scheduleDelete()}
            >
              Schedule deletion (24h)
            </Button>
          </>
        )}
      </section>
    </Dialog>
  );
}

function Stat({
  title,
  value,
  progress,
}: {
  title: string;
  value: string;
  progress?: number | null;
}) {
  return (
    <div className="bg-surface-0 border border-border-subtle rounded p-2">
      <div className="text-fg-muted">{title}</div>
      <div className="font-medium">{value}</div>
      {progress != null && (
        <div
          className="mt-1 h-1 bg-surface-2 rounded overflow-hidden"
          role="progressbar"
          aria-valuenow={Math.round(progress)}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={`${title} usage`}
        >
          <div
            className={`h-full ${progress > 90 ? "bg-danger" : progress > 70 ? "bg-warning" : "bg-accent"}`}
            style={{ width: `${progress}%` }}
          />
        </div>
      )}
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block text-xs space-y-1">
      <span className="text-fg-muted">{label}</span>
      <div>{children}</div>
    </label>
  );
}
