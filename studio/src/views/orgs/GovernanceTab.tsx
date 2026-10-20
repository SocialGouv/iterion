import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Spinner } from "@/components/ui/Spinner";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { useConfirm } from "@/hooks/useConfirm";

import {
  type ProvisionApproval,
  approveProvision,
  getOrgRoutingPolicy,
  getOrgSettings,
  listOrgProvisionApprovals,
  listOrgTeamSummaries,
  putOrgRoutingPolicy,
  rejectProvision,
  updateOrgSettings,
  updateOrgTeamCaps,
} from "@/api/orgGovernance";
import RoutingPolicySection from "./RoutingPolicySection";

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
  // A failed fetch must never render as an empty-but-healthy tab ("None."
  // over a queue that exists) — mutation errors win the shared banner,
  // else whichever query failed.
  const fetchErr =
    settingsQuery.error ?? approvalsQuery.error ?? teamsQuery.error ?? null;
  const banner = err ?? (fetchErr ? errorMessage(fetchErr) : null);

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
      {banner && (
        <InlineBanner tone="danger" layout="inline">
          {banner}
        </InlineBanner>
      )}

      <RoutingPolicySection
        scope="org"
        id={orgID}
        canManage={canManage}
        getPolicy={getOrgRoutingPolicy}
        putPolicy={putOrgRoutingPolicy}
      />

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
          <Checkbox
            label="Require org approval for repo provisioning"
            checked={settings.require_provision_approval}
            disabled={!canManage}
            onChange={(e) => void toggleApprovalFlag(e.target.checked)}
          />
        )}
      </section>

      {canManage && (
        <section>
          <h3 className="font-medium mb-2">Pending provisioning requests</h3>
          {approvalsQuery.error ? (
            <div className="text-fg-muted text-sm">Could not load the queue — see the error above.</div>
          ) : approvals.length === 0 ? (
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
// list, including the shapes that RETARGET the record: a replace request
// makes the listed bots the exact set (the dropped ones are disabled),
// and launch_vars_replace makes the spoken map the whole set of pins.
// Empty string when the request carries none.
function approvalExtras(a: ProvisionApproval): string {
  const parts: string[] = [];
  if (a.replace) {
    const kept = new Set(a.bot_ids);
    const dropped = (a.base_bot_ids ?? []).filter((b) => !kept.has(b));
    parts.push(
      dropped.length > 0
        ? `replaces the bot set — disables: ${dropped.join(", ")}`
        : "replaces the bot set (the listed bots become the exact set)",
    );
  }
  // The spoken-empty gestures. Spoken-ness has two witnesses on this wire:
  // the Set fact (records parked since the flags shipped) and the field
  // itself — null means silent, an empty-but-present value IS the gesture
  // (a legacy record parked before the flags carried the empty value and
  // no fact). Either witness renders the gesture the approval executes.
  if (a.auto_fix === true) parts.push("zero-touch auto-fix ON");
  else if (a.auto_fix === false) parts.push("zero-touch auto-fix OFF");
  const holdEmpty = (a.hold_labels ?? []).length === 0;
  if (holdEmpty && (a.hold_labels_set || a.hold_labels != null)) {
    // The whole-pause lift re-arms exactly the labels the base recorded —
    // name them when the base is on the record, the partial branch's
    // rationale: the bare claim reads as the whole story.
    const lifted = a.base_hold_labels ?? [];
    parts.push(
      "hold labels: lifts the automation pause" +
        (lifted.length > 0 ? ` — lifts: ${lifted.join(", ")}` : ""),
    );
  } else if (a.hold_labels && a.hold_labels.length > 0) {
    // A base recorded means the change acts on a stored set: name what it
    // lifts — a partial lift parks precisely because of the label it
    // removes from the pause, and the destination list alone reads as the
    // whole story. Absent base (new repo, empty stored set): nothing can
    // be lifted, so no delta claim.
    const spoken = a.hold_labels;
    const lifted = (a.base_hold_labels ?? []).filter((l) => !spoken.includes(l));
    parts.push(
      `hold labels: ${spoken.join(", ")}` +
        (lifted.length > 0 ? ` — lifts: ${lifted.join(", ")}` : ""),
    );
  }
  const allowEmpty = (a.label_allowlist ?? []).length === 0;
  if (allowEmpty && (a.label_allowlist_set || a.label_allowlist != null)) {
    // Same delta rule as the pause lift: the widen re-arms exactly the
    // stored allowlist.
    const widened = a.base_label_allowlist ?? [];
    parts.push(
      "label allowlist: widens dispatch to any label" +
        (widened.length > 0 ? ` — widens to: ${widened.join(", ")}` : ""),
    );
  } else if (a.label_allowlist && a.label_allowlist.length > 0) {
    const widened = a.label_allowlist.filter((l) => !(a.base_label_allowlist ?? []).includes(l));
    parts.push(
      `label allowlist: ${a.label_allowlist.join(", ")}` +
        (widened.length > 0 ? ` — widens to: ${widened.join(", ")}` : ""),
    );
  }
  const mapSpoken = a.launch_vars_set || a.launch_vars != null;
  const mapEmpty = Object.keys(a.launch_vars ?? {}).length === 0;
  if (mapSpoken && mapEmpty) {
    // A spoken empty map IS the whole-map clear — replace flag or not,
    // Set fact or legacy empty-but-present value.
    parts.push("launch vars: replaces the whole map — clears every existing pin");
  } else if (a.launch_vars) {
    // The map the replay applies, whatever parked the record: replace
    // makes the spoken map the whole set of pins, a legacy (flagless)
    // map applies exactly the same way.
    parts.push(
      a.launch_vars_replace
        ? `launch vars replace the whole map: ${Object.entries(a.launch_vars)
            .map(([k, v]) => `${k}=${v}`)
            .join(", ")}`
        : `launch vars: ${Object.entries(a.launch_vars)
            .map(([k, v]) => `${k}=${v}`)
            .join(", ")}`,
    );
  } else if (a.launch_vars_replace) {
    // Replace acknowledged with NO map on the wire (null or absent): the
    // server treats the record as silent and keeps the stored pins — say
    // that, not a clear.
    parts.push("launch vars: replace requested but no map spoken — the stored pins stay");
  }
  if (a.schedule_crons && Object.keys(a.schedule_crons).length > 0)
    parts.push(
      `schedules: ${Object.entries(a.schedule_crons)
        .map(([k, v]) => `${k} @ ${v}`)
        .join(", ")}`,
    );
  if (a.overlap) parts.push(`overlap: ${a.overlap}`);
  const routing = routingSummary(a);
  if (routing) parts.push(routing);
  return parts.join(" · ");
}

// routingSummary renders the routing block a parked request speaks about —
// the payload this gate exists to arbitrate — as one line: the pin(s) it
// applies (selector → backend[/model][/provider], with the effort slot
// when the pin carries one) followed by every non-override cell verbatim
// (the providers allowlist, strict, pair_order, triggers, model_classes,
// cross_harness, refused_pinned_key — whatever the block carries), or
// CLEARED when the request wipes a previously approved block. "" when the
// request says nothing about routing (routing_set absent).
function routingSummary(a: ProvisionApproval): string {
  if (!a.routing_set) return "";
  if (!a.routing) return "routing: CLEARED";
  const routing = a.routing as Record<string, unknown>;
  const ov = routing.overrides;
  if (Array.isArray(ov) && ov.length > 0) {
    const pins = ov.map((o) => {
      const rec = (o ?? {}) as Record<string, unknown>;
      const sel = typeof rec.selector === "string" ? rec.selector : "?";
      const target =
        [rec.backend, rec.model, rec.provider]
          .filter((v): v is string => typeof v === "string")
          .join("/") || "?";
      const effort =
        typeof rec.effort === "string" && rec.effort !== ""
          ? ` (effort: ${rec.effort})`
          : "";
      return `${sel} → ${target}${effort}`;
    });
    const rest = { ...routing };
    delete rest.overrides;
    const extra = Object.keys(rest).length > 0 ? `, ${JSON.stringify(rest)}` : "";
    return `routing: ${pins.join(", ")}${extra}`;
  }
  return `routing: ${JSON.stringify(a.routing)}`;
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
    // Submit ONLY the fields that changed: the server validates the
    // platform ceiling per submitted field, so re-sending an unchanged
    // stored value (e.g. a super-admin cap above the ceiling) would get
    // this row rejected for a field the operator never touched.
    const patch: { max_concurrent_runs?: number; launch_rate_per_min?: number } = {};
    if (runsN !== (team.max_concurrent_runs ?? 0)) patch.max_concurrent_runs = runsN;
    if (rateN !== (team.launch_rate_per_min ?? 0)) patch.launch_rate_per_min = rateN;
    if (Object.keys(patch).length === 0) return;
    setBusy(true);
    try {
      await updateOrgTeamCaps(orgID, team.id, patch);
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
