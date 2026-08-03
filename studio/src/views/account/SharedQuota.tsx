import { errorMessage } from "@/lib/errorHints";
import { formatCost, formatDateTime } from "@/lib/format";
import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Meter } from "@/components/ui/Meter";
import { TBody, THead, Table, Td, Th, Tr } from "@/components/ui/Table";
import PanelLoading from "@/components/shared/PanelLoading";
import { useConfirm } from "@/hooks/useConfirm";
import { useUIStore } from "@/store/ui";
import type { OAuthKind } from "@/api/byok";
import {
  listMyPledges,
  listMyPoolHistory,
  savePledge,
  withdrawPledge,
  type PledgeInput,
  type PledgeLease,
  type PledgeStatus,
  type PledgeView,
  type PoolLimits,
} from "@/api/credpool";

const KIND_LABEL: Record<OAuthKind, string> = {
  claude_code: "Claude Pro / Max",
  codex: "ChatGPT (Codex)",
};

// The same caveat the org-shared forfait already carries. Lending a
// personal subscription to other people goes a step further than sharing
// one inside an org, so the wording stays in front of the donor rather
// than buried in docs.
const TOS_NOTICE =
  "A Claude or ChatGPT subscription is an individual licence. Sharing yours is a convenience for developing and testing bots — not a production-automation credential. You stay in control: the ceilings below are yours, and pausing takes effect on the next run.";

const STATUS_COPY: Record<PledgeStatus, { label: string; tone: "success" | "warning" | "neutral" | "danger" }> = {
  active: { label: "Sharing", tone: "success" },
  paused: { label: "Paused", tone: "neutral" },
  cooling: { label: "Resting until your quota window reopens", tone: "warning" },
  out_of_hours: { label: "Outside your sharing hours", tone: "neutral" },
  exhausted: { label: "Ceiling reached for now", tone: "warning" },
  unhealthy: { label: "Needs reconnecting", tone: "danger" },
  bot_filtered: { label: "Sharing, but not with this bot", tone: "neutral" },
};

/** Blank input = "no limit on this axis", which the API encodes as 0. */
function numOrZero(s: string): number {
  const n = Number(s);
  return Number.isFinite(n) && n > 0 ? n : 0;
}
const showNum = (v?: number) => (v && v > 0 ? String(v) : "");

export default function SharedQuota() {
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["my-pledges"], queryFn: listMyPledges });
  const history = useQuery<PledgeLease[]>({
    queryKey: ["my-pool-history"],
    queryFn: listMyPoolHistory,
  });
  const [err, setErr] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();
  const addToast = useUIStore((s) => s.addToast);

  const pledges = query.data?.pledges ?? [];
  const poolAvailable = Boolean(query.data?.pool_id);

  const reload = () => {
    setErr(null);
    void queryClient.invalidateQueries({ queryKey: ["my-pledges"] });
    void queryClient.invalidateQueries({ queryKey: ["my-pool-history"] });
  };

  const onSaved = (message: string) => {
    addToast(message, "success");
    reload();
  };

  return (
    <div className="space-y-4">
      {dialog}
      <div>
        <h2 className="text-lg font-semibold">Share my quota</h2>
        <p className="text-sm text-fg-muted mt-1">
          Lend the unused part of your Claude or ChatGPT subscription so other runs on this
          instance can use it. You set the ceilings, you see everything it ran, and you can stop
          at any moment.
        </p>
      </div>

      <InlineBanner tone="warning" layout="inline">
        {TOS_NOTICE}
      </InlineBanner>

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {!poolAvailable && !query.isLoading && (
        <InlineBanner tone="info" layout="inline">
          No credential pool accepts contributions on this instance yet — ask an operator to
          enable one for your workspace.
        </InlineBanner>
      )}

      {query.isFetching && pledges.length === 0 ? (
        <PanelLoading />
      ) : (
        <div className="space-y-4">
          {(["claude_code", "codex"] as OAuthKind[]).map((kind) => (
            <PledgeCard
              key={kind}
              kind={kind}
              pledge={pledges.find((p) => p.kind === kind)}
              disabled={!poolAvailable}
              onError={setErr}
              onSaved={onSaved}
              confirm={confirm}
            />
          ))}
        </div>
      )}

      <HistoryTable leases={history.data ?? []} />
    </div>
  );
}

function PledgeCard({
  kind,
  pledge,
  disabled,
  onError,
  onSaved,
  confirm,
}: {
  kind: OAuthKind;
  pledge?: PledgeView;
  disabled: boolean;
  onError: (msg: string | null) => void;
  onSaved: (msg: string) => void;
  confirm: ReturnType<typeof useConfirm>["confirm"];
}) {
  const [limits, setLimits] = useState<PoolLimits>({});
  const [hours, setHours] = useState<{ start: string; end: string }>({ start: "", end: "" });
  const [busy, setBusy] = useState(false);

  // Re-seed the form whenever the server's view changes, so a save or a
  // reload shows what is actually stored rather than stale local edits.
  useEffect(() => {
    setLimits(pledge?.limits ?? {});
    setHours({
      start: pledge?.window ? String(pledge.window.start_hour) : "",
      end: pledge?.window ? String(pledge.window.end_hour) : "",
    });
  }, [pledge]);

  const status = pledge?.status;
  const copy = status ? STATUS_COPY[status] : null;

  const buildInput = (enabled: boolean): PledgeInput => {
    const start = numOrZero(hours.start);
    const end = numOrZero(hours.end);
    return {
      enabled,
      limits,
      // Both blank (or equal) means "any time" — send no window at all
      // rather than a 0→0 range the reader would have to special-case.
      window:
        hours.start === "" && hours.end === ""
          ? null
          : {
              start_hour: start,
              end_hour: end,
              timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
            },
      bots: pledge?.bots,
    };
  };

  const save = async (enabled: boolean) => {
    setBusy(true);
    onError(null);
    try {
      await savePledge(kind, buildInput(enabled));
      onSaved(enabled ? `Sharing your ${KIND_LABEL[kind]} quota.` : "Sharing paused.");
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const withdraw = async () => {
    const ok = await confirm({
      title: "Stop sharing?",
      message:
        "Your contribution is withdrawn and no new run will use it. Runs already in flight finish on the credential they were granted.",
      confirmLabel: "Stop sharing",
      confirmVariant: "danger",
    });
    if (!ok) return;
    onError(null);
    try {
      await withdrawPledge(kind);
      onSaved("Contribution withdrawn.");
    } catch (e) {
      onError(errorMessage(e));
    }
  };

  return (
    <div className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
      <div className="flex items-center justify-between gap-2">
        <div>
          <h3 className="font-medium">{KIND_LABEL[kind]}</h3>
          {pledge && !pledge.connected && (
            <div className="text-xs text-fg-muted">
              Subscription no longer connected — reconnect it under “OAuth subscriptions”.
            </div>
          )}
        </div>
        {copy ? <Badge variant={copy.tone}>{copy.label}</Badge> : <Badge variant="neutral">Not shared</Badge>}
      </div>

      {pledge?.health_detail && (
        <InlineBanner tone="warning" layout="inline">
          {pledge.health_detail}
        </InlineBanner>
      )}

      {pledge && (
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Meter
            label="Given today"
            value={pledge.today.cost_usd}
            max={pledge.limits.max_usd_per_day}
            formatValue={formatCost}
            formatMax={formatCost}
            size="sm"
            hint="Estimated: a subscription bills nothing per call, so this is derived from token counts."
          />
          <Meter
            label="Given this week"
            value={pledge.this_week.cost_usd}
            max={pledge.limits.max_usd_per_week}
            formatValue={formatCost}
            formatMax={formatCost}
            size="sm"
            hint="Estimated, same caveat as the daily figure."
          />
          <div className="text-xs text-fg-muted">
            {pledge.today.runs} run(s) served today
            {pledge.last_served_at && ` · last ${formatDateTime(pledge.last_served_at)}`}
          </div>
          {pledge.cooldown_until && (
            <div className="text-xs text-fg-muted">
              Resting until {formatDateTime(pledge.cooldown_until)}
            </div>
          )}
        </div>
      )}

      <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
        <div>
          <FieldLabel help="Blank = no daily ceiling.">Max $ / day</FieldLabel>
          <Input
            inputMode="decimal"
            placeholder="no limit"
            value={showNum(limits.max_usd_per_day)}
            onChange={(e) => setLimits({ ...limits, max_usd_per_day: numOrZero(e.target.value) })}
          />
        </div>
        <div>
          <FieldLabel help="Blank = no weekly ceiling. Must not be below the daily one.">
            Max $ / week
          </FieldLabel>
          <Input
            inputMode="decimal"
            placeholder="no limit"
            value={showNum(limits.max_usd_per_week)}
            onChange={(e) => setLimits({ ...limits, max_usd_per_week: numOrZero(e.target.value) })}
          />
        </div>
        <div>
          <FieldLabel help="Blank = no cap on how many runs you serve per day.">
            Max runs / day
          </FieldLabel>
          <Input
            inputMode="numeric"
            placeholder="no limit"
            value={showNum(limits.max_runs_per_day)}
            onChange={(e) => setLimits({ ...limits, max_runs_per_day: numOrZero(e.target.value) })}
          />
        </div>
        <div>
          <FieldLabel help="Keeps your own sessions responsive. Blank = no cap.">
            Max runs at once
          </FieldLabel>
          <Input
            inputMode="numeric"
            placeholder="no limit"
            value={showNum(limits.max_concurrent_runs)}
            onChange={(e) =>
              setLimits({ ...limits, max_concurrent_runs: numOrZero(e.target.value) })
            }
          />
        </div>
      </div>

      <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
        <div>
          <FieldLabel help="Local time. Leave both blank to share around the clock; 19 → 8 shares overnight.">
            Share from (hour)
          </FieldLabel>
          <Input
            inputMode="numeric"
            placeholder="any"
            value={hours.start}
            onChange={(e) => setHours({ ...hours, start: e.target.value })}
          />
        </div>
        <div>
          <FieldLabel>Until (hour)</FieldLabel>
          <Input
            inputMode="numeric"
            placeholder="any"
            value={hours.end}
            onChange={(e) => setHours({ ...hours, end: e.target.value })}
          />
        </div>
      </div>

      <div className="flex flex-wrap gap-2 items-center">
        <Button variant="primary" disabled={disabled || busy} onClick={() => void save(true)}>
          {pledge?.enabled ? "Update terms" : "Start sharing"}
        </Button>
        {pledge?.enabled && (
          <Button variant="secondary" disabled={busy} onClick={() => void save(false)}>
            Pause sharing
          </Button>
        )}
        {pledge && (
          <Button variant="danger" disabled={busy} onClick={() => void withdraw()}>
            Stop &amp; withdraw
          </Button>
        )}
      </div>
    </div>
  );
}

function HistoryTable({ leases }: { leases: PledgeLease[] }) {
  if (leases.length === 0) return null;
  return (
    <section className="space-y-2">
      <h3 className="font-medium text-sm">What your quota ran</h3>
      <div className="border border-border-subtle rounded">
        <Table caption="Runs served by your shared quota" density="sm">
          <THead className="bg-surface-1">
            <Th>When</Th>
            <Th>Bot</Th>
            <Th>Requested by</Th>
            <Th align="right">Cost (est.)</Th>
            <Th>Outcome</Th>
          </THead>
          <TBody>
            {leases.map((l) => (
              <Tr key={`${l.run_id}-${l.acquired_at}`}>
                <Td className="whitespace-nowrap">{formatDateTime(l.acquired_at)}</Td>
                <Td>{l.bot_id || "—"}</Td>
                <Td>{l.requester_id || "—"}</Td>
                <Td align="right">{l.closed ? formatCost(l.cost_usd) : "running…"}</Td>
                <Td>{l.outcome || (l.closed ? "ok" : "")}</Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      </div>
    </section>
  );
}
