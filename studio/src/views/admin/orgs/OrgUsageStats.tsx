// Usage stat grid for the org drawer — members / memory / runs / cost plus
// resource counters, with quota progress bars where a cap applies. "—"
// placeholders render while the usage query is still loading (or failed).

import { fmtBytes, fmtUSD, pct, type OrgUsage } from "@/api/usage";

import { Stat } from "./orgFields";

export function OrgUsageStats({ usage }: { usage: OrgUsage | null }) {
  return (
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
  );
}
