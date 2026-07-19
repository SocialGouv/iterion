import { errorMessage } from "@/lib/errorHints";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { InlineBanner } from "@/components/ui/InlineBanner";

import {
  FeatureUnavailableError,
  type OrgUsage,
  fmtBytes,
  fmtUSD,
  getOrgUsage,
  pct,
} from "@/api/usage";
import { EmptyState } from "@/components/ui/EmptyState";
import { Tooltip } from "@/components/ui/Tooltip";
import PanelLoading from "@/components/shared/PanelLoading";

interface Props {
  orgID: string;
}

export default function UsageTab({ orgID }: Props) {
  const query = useQuery<OrgUsage>({
    queryKey: ["org-usage", orgID],
    queryFn: () => getOrgUsage(orgID),
    // An org switch keeps the previous org's figures on screen while the
    // new ones load, instead of dropping back to the whole-tab panel.
    placeholderData: keepPreviousData,
  });
  const usage = query.data ?? null;
  const unavailable = query.error instanceof FeatureUnavailableError;
  const err =
    query.error && !unavailable && !query.isFetching
      ? errorMessage(query.error)
      : null;

  if (unavailable) {
    return (
      <EmptyState
        title="Usage not enabled on this server"
        message="Per-org usage metering requires the cloud-mode metric store."
      />
    );
  }
  if (err) {
    return (
      <InlineBanner tone="danger" layout="inline">
        {err}
      </InlineBanner>
    );
  }
  if (!usage) {
    return (
      <PanelLoading />
    );
  }

  return (
    <div className="space-y-6">
      <section className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
        <UsageCard
          title="Runs this month"
          used={usage.runs_this_month}
          quota={usage.monthly_run_quota}
          fmt={(n) => String(n)}
          quotaHelp="Monthly run quota"
        />
        <UsageCard
          title="Cost this month"
          used={usage.cost_usd_this_month}
          quota={usage.monthly_cost_cap_usd}
          fmt={fmtUSD}
          quotaHelp="Monthly spend cap (USD)"
        />
        <UsageCard
          title="Active runs"
          used={usage.active_runs}
          quota={usage.max_concurrent_runs}
          fmt={(n) => String(n)}
          quotaHelp="Max concurrent runs"
        />
        <UsageCard
          title="Memory used"
          used={usage.memory_used_bytes}
          quota={usage.effective_memory_quota_bytes}
          fmt={fmtBytes}
          quotaHelp="Shared-memory ceiling (effective)"
        />
        <UsageCard
          title="Webhook calls this month"
          used={usage.webhook_calls_this_month}
          fmt={(n) => String(n)}
        />
        <UsageCard
          title="Input tokens this month"
          used={usage.input_tokens_this_month}
          fmt={(n) => n.toLocaleString()}
        />
        <UsageCard
          title="Output tokens this month"
          used={usage.output_tokens_this_month}
          fmt={(n) => n.toLocaleString()}
        />
        <UsageCard
          title="Members"
          used={usage.members}
          fmt={(n) => String(n)}
        />
      </section>

      <section className="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
        <CountCard title="API keys" n={usage.api_key_count} />
        <CountCard title="Team secrets" n={usage.generic_secret_count} />
        <CountCard title="Bot bindings" n={usage.bot_binding_count} />
        <CountCard title="Webhooks" n={usage.webhook_count} />
      </section>
    </div>
  );
}

function UsageCard({
  title,
  used,
  quota,
  fmt,
  quotaHelp,
}: {
  title: string;
  used: number | undefined;
  quota?: number;
  fmt: (n: number) => string;
  quotaHelp?: string;
}) {
  const usedN = used ?? 0;
  const p = pct(usedN, quota);
  const undefinedField = used == null;
  return (
    <div className="bg-surface-1 border border-border-subtle rounded p-3 space-y-1">
      <div className="text-xs uppercase tracking-wider text-fg-muted">{title}</div>
      <div className="flex items-baseline gap-1">
        {undefinedField ? (
          <Tooltip content="Not reported by this server.">
            <span className="text-2xl font-semibold">—</span>
          </Tooltip>
        ) : (
          <span className="text-2xl font-semibold">{fmt(usedN)}</span>
        )}
        {quota != null && quota > 0 && (
          <Tooltip content={quotaHelp ?? "Quota"}>
            <span className="text-xs text-fg-muted">/ {fmt(quota)}</span>
          </Tooltip>
        )}
      </div>
      {p != null && (
        <div
          className="h-1.5 bg-surface-2 rounded overflow-hidden"
          role="progressbar"
          aria-valuenow={Math.round(p)}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={`${title} usage`}
        >
          <div
            className={`h-full ${p > 90 ? "bg-danger" : p > 70 ? "bg-warning" : "bg-accent"}`}
            style={{ width: `${p}%` }}
          />
        </div>
      )}
    </div>
  );
}

function CountCard({ title, n }: { title: string; n: number | undefined }) {
  return (
    <div className="bg-surface-1 border border-border-subtle rounded p-2">
      <div className="text-fg-muted">{title}</div>
      <div className="text-lg font-semibold">{n ?? 0}</div>
    </div>
  );
}
