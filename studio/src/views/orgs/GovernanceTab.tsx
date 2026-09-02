import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Spinner } from "@/components/ui/Spinner";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { useConfirm } from "@/hooks/useConfirm";

import {
  type ProvisionApproval,
  approveProvision,
  getOrgSettings,
  listOrgProvisionApprovals,
  listOrgTeamSummaries,
  rejectProvision,
  updateOrgSettings,
  updateOrgTeamCaps,
} from "@/api/orgGovernance";

// GovernanceTab groups the org-admin-run controls: the ex-ante
// provisioning approval flag + its pending queue, and the per-team
// executor caps delegated from the platform console. The org-level
// monthly budget stays under Plan + quotas (super-admin managed).
export default function GovernanceTab({
  orgID,
  canManage,
}: {
  orgID: string;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const [err, setErr] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();

  const settingsQuery = useQuery({
    queryKey: ["org-settings", orgID],
    queryFn: () => getOrgSettings(orgID),
  });
  const approvalsQuery = useQuery({
    queryKey: ["org-provision-approvals", orgID],
    queryFn: () => listOrgProvisionApprovals(orgID),
    enabled: canManage,
  });
  const teamsQuery = useQuery({
    queryKey: ["org-team-summaries", orgID],
    queryFn: () => listOrgTeamSummaries(orgID),
  });

  const settings = settingsQuery.data ?? null;
  const approvals = approvalsQuery.data ?? [];
  const teams = teamsQuery.data ?? [];

  const reload = () => {
    setErr(null);
    void queryClient.invalidateQueries({ queryKey: ["org-settings", orgID] });
    void queryClient.invalidateQueries({ queryKey: ["org-provision-approvals", orgID] });
    void queryClient.invalidateQueries({ queryKey: ["org-team-summaries", orgID] });
  };

  const toggleApprovalFlag = async (next: boolean) => {
    try {
      await updateOrgSettings(orgID, { require_provision_approval: next });
      reload();
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  const approve = async (a: ProvisionApproval) => {
    const extras = approvalExtras(a);
    const ok = await confirm({
      title: "Approve provisioning?",
      message: `Enable ${a.bot_ids.join(", ")} on ${a.repo_full_name}${extras ? ` (${extras})` : ""}? The webhook and token are created on the forge immediately.`,
      confirmLabel: "Approve",
    });
    if (!ok) return;
    try {
      await approveProvision(orgID, a.id);
      reload();
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  const reject = async (a: ProvisionApproval) => {
    const ok = await confirm({
      title: "Reject provisioning?",
      message: `Reject the request to enable ${a.bot_ids.join(", ")} on ${a.repo_full_name}? The requesting team admin can submit a new one.`,
      confirmLabel: "Reject",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await rejectProvision(orgID, a.id);
      reload();
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

      <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-2">
        <h3 className="font-medium">Provisioning approval</h3>
        <p className="text-xs text-fg-muted">
          When on, a team admin's request to connect a repo to a bot (or add a
          bot to a connected repo) waits for an org admin's approval — nothing
          is created on the forge until approved. Org admins provision
          directly.
        </p>
        {settings === null ? (
          <Spinner size="sm" label="Loading settings" />
        ) : (
          <label className="inline-flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={settings.require_provision_approval}
              disabled={!canManage}
              onChange={(e) => void toggleApprovalFlag(e.target.checked)}
            />
            Require org approval for repo provisioning
          </label>
        )}
      </section>

      {canManage && (
        <section>
          <h3 className="font-medium mb-2">Pending provisioning requests</h3>
          {approvals.length === 0 ? (
            <div className="text-fg-muted text-sm">None.</div>
          ) : (
            <Table caption="Pending provisioning requests">
              <THead>
                <Th>Repository</Th>
                <Th>Bots</Th>
                <Th>Team</Th>
                <Th>Requested by</Th>
                <Th>Requested</Th>
                <Th align="right" srLabel="Actions" />
              </THead>
              <TBody>
                {approvals.map((a) => (
                  <Tr key={a.id}>
                    <Td className="font-mono text-xs">{a.repo_full_name}</Td>
                    <Td>
                      {a.bot_ids.join(", ")}
                      {approvalExtras(a) && (
                        <div className="text-caption text-fg-muted">{approvalExtras(a)}</div>
                      )}
                    </Td>
                    <Td className="font-mono text-xs">{a.tenant_id}</Td>
                    <Td className="font-mono text-xs">{a.requested_by}</Td>
                    <Td className="text-fg-muted">{formatDateTime(a.created_at)}</Td>
                    <Td align="right">
                      <div className="inline-flex gap-2">
                        <Button variant="primary" size="sm" onClick={() => void approve(a)}>
                          Approve
                        </Button>
                        <Button variant="danger" size="sm" onClick={() => void reject(a)}>
                          Reject
                        </Button>
                      </div>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
          )}
        </section>
      )}

      <section>
        <h3 className="font-medium mb-2">Per-team usage caps</h3>
        <p className="text-xs text-fg-muted mb-2">
          Executor caps enforced at launch, per team. 0 inherits the platform
          default. The org-wide monthly budget lives under Plan + quotas and is
          managed by platform admins.
        </p>
        <Table caption="Per-team usage caps">
          <THead>
            <Th>Team</Th>
            <Th>Max concurrent runs</Th>
            <Th>Launches / min</Th>
            {canManage && <Th align="right" srLabel="Actions" />}
          </THead>
          <TBody>
            {teams.map((t) => (
              <TeamCapsRow
                key={t.id}
                orgID={orgID}
                team={t}
                canManage={canManage}
                onSaved={reload}
                onError={setErr}
              />
            ))}
          </TBody>
        </Table>
      </section>
    </div>
  );
}

// approvalExtras summarises the automation switches the approval would
// replay verbatim — what the admin is REALLY approving beyond the bot
// list. Empty string when the request carries none.
function approvalExtras(a: ProvisionApproval): string {
  const parts: string[] = [];
  if (a.auto_fix) parts.push("zero-touch auto-fix ON");
  if (a.hold_labels && a.hold_labels.length > 0)
    parts.push(`hold labels: ${a.hold_labels.join(", ")}`);
  if (a.label_allowlist && a.label_allowlist.length > 0)
    parts.push(`label allowlist: ${a.label_allowlist.join(", ")}`);
  if (a.launch_vars && Object.keys(a.launch_vars).length > 0)
    parts.push(
      `launch vars: ${Object.entries(a.launch_vars)
        .map(([k, v]) => `${k}=${v}`)
        .join(", ")}`,
    );
  if (a.schedule_crons && Object.keys(a.schedule_crons).length > 0)
    parts.push(
      `schedules: ${Object.entries(a.schedule_crons)
        .map(([k, v]) => `${k} @ ${v}`)
        .join(", ")}`,
    );
  if (a.overlap) parts.push(`overlap: ${a.overlap}`);
  return parts.join(" · ");
}

function TeamCapsRow({
  orgID,
  team,
  canManage,
  onSaved,
  onError,
}: {
  orgID: string;
  team: { id: string; name: string; max_concurrent_runs?: number; launch_rate_per_min?: number };
  canManage: boolean;
  onSaved: () => void;
  onError: (m: string) => void;
}) {
  const [runs, setRuns] = useState(String(team.max_concurrent_runs ?? 0));
  const [rate, setRate] = useState(String(team.launch_rate_per_min ?? 0));
  const [busy, setBusy] = useState(false);
  const dirty =
    runs !== String(team.max_concurrent_runs ?? 0) ||
    rate !== String(team.launch_rate_per_min ?? 0);

  const save = async () => {
    const runsN = Number(runs);
    const rateN = Number(rate);
    if (!Number.isInteger(runsN) || runsN < 0 || !Number.isInteger(rateN) || rateN < 0) {
      onError("Caps must be non-negative integers (0 = platform default).");
      return;
    }
    setBusy(true);
    try {
      await updateOrgTeamCaps(orgID, team.id, {
        max_concurrent_runs: runsN,
        launch_rate_per_min: rateN,
      });
      onSaved();
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Tr>
      <Td>{team.name}</Td>
      <Td>
        {canManage ? (
          <Input
            size="sm"
            type="number"
            min={0}
            value={runs}
            onChange={(e) => setRuns(e.target.value)}
            aria-label={`Max concurrent runs for ${team.name}`}
            className="w-24"
          />
        ) : (
          (team.max_concurrent_runs ?? 0) || "default"
        )}
      </Td>
      <Td>
        {canManage ? (
          <Input
            size="sm"
            type="number"
            min={0}
            value={rate}
            onChange={(e) => setRate(e.target.value)}
            aria-label={`Launch rate per minute for ${team.name}`}
            className="w-24"
          />
        ) : (
          (team.launch_rate_per_min ?? 0) || "default"
        )}
      </Td>
      {canManage && (
        <Td align="right">
          <Button size="sm" onClick={() => void save()} disabled={!dirty || busy}>
            {busy ? <Spinner size="sm" /> : "Save"}
          </Button>
        </Td>
      )}
    </Tr>
  );
}
