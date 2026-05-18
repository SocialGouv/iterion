import { useQuery } from "@tanstack/react-query";

import { previewRunCost } from "@/api/runs";
import { Tooltip } from "@/components/ui/Tooltip";

interface Props {
  // The .iter file path the workflow was loaded from. The server uses
  // it as a parser anchor and (in local mode) as a filesystem fallback
  // when `source` is empty.
  filePath?: string;
  // Inline workflow body. Wins over filePath when both are set, matching
  // the launch endpoint's precedence.
  source?: string;
}

const fmtUSD = (v: number): string => {
  if (v >= 1) return `$${v.toFixed(2)}`;
  if (v >= 0.01) return `$${v.toFixed(2)}`;
  return `$${v.toFixed(3)}`;
};

const fmtTokens = (n: number): string => {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1000)}k`;
  return String(n);
};

// CostPreviewChip surfaces an inline best-effort cost estimate next to
// the Launch button so the operator catches accidental five-dollar
// runs before clicking. Silently hidden when:
//   - the endpoint hasn't responded yet (initial fetch)
//   - the workflow has no LLM nodes (notes contains "no_llm_nodes")
//   - none of the resolved models has pricing data ("no_pricing_data")
//   - the source can't be parsed yet ("workflow_unparseable")
//   - the fetch fails (network / 5xx)
// The chip is decoration, not a gate; the Launch button stays the
// authoritative validation surface.
export default function CostPreviewChip({ filePath, source }: Props) {
  const enabled = (source ?? "").length > 0 || (filePath ?? "").length > 0;
  const { data, isLoading, error } = useQuery({
    queryKey: ["preview-cost", filePath, source],
    queryFn: () => previewRunCost({ file_path: filePath, source }),
    enabled,
    staleTime: 30_000,
    // Cost estimates are advisory — don't surface transient network
    // errors as toasts or banners. The chip just stays hidden.
    retry: false,
  });

  if (isLoading || error || !data) return null;
  if (data.nodes.length === 0) return null;
  if (data.notes?.includes("no_pricing_data")) return null;

  const range =
    data.cost_min_usd > 0
      ? `${fmtUSD(data.cost_min_usd)}–${fmtUSD(data.cost_max_usd)}`
      : fmtUSD(data.cost_max_usd);
  const tokens = fmtTokens(Math.round((data.tokens_min + data.tokens_max) / 2));

  const tooltip = (
    <div className="space-y-1">
      <div className="font-medium text-fg-default">Estimated run cost</div>
      <div className="text-[11px] text-fg-muted">
        Range covers retries + plausible second-pass loops. Pricing is best-effort
        — actual cost depends on real token usage.
      </div>
      <ul className="space-y-0.5 mt-1.5">
        {data.nodes.map((n) => (
          <li key={n.node_id} className="flex items-baseline gap-2 text-[11px]">
            <span className="font-mono text-fg-default truncate max-w-[140px]">
              {n.node_id}
            </span>
            <span className="text-fg-subtle">{n.kind}</span>
            {n.model && (
              <span className="text-fg-subtle truncate max-w-[160px]">
                {n.model}
              </span>
            )}
            <span className="ml-auto text-fg-muted tabular-nums">
              {n.cost_min_usd > 0 ? fmtUSD(n.cost_min_usd) : "—"}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );

  return (
    <Tooltip content={tooltip} side="top">
      <span
        className="inline-flex items-center text-[10px] px-1.5 py-0.5 rounded border bg-surface-2 text-fg-muted border-border-default tabular-nums cursor-help"
        aria-label={`Estimated cost ${range} for approximately ${tokens} tokens`}
      >
        ≈ {range} · {tokens} tok
      </span>
    </Tooltip>
  );
}
