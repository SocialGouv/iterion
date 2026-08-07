import { useQuery } from "@tanstack/react-query";

import { previewRunCost } from "@/api/runs";
import { Tooltip } from "@/components/ui/Tooltip";
import { formatCost, formatTokens } from "@/lib/format";

interface Props {
  filePath?: string;
  source?: string;
}

// Cheap, non-cryptographic source digest for the react-query cache key.
// Stringifying the entire workflow source as a key causes lookups to
// stringify-then-compare on every render; the digest collapses that to
// a fixed-length number while still invalidating on real edits.
function digest(s: string | undefined): number {
  if (!s) return 0;
  let h = 0x811c9dc5; // FNV-1a 32-bit seed
  for (let i = 0; i < s.length; i++) {
    h = ((h ^ s.charCodeAt(i)) * 0x01000193) >>> 0;
  }
  return h;
}

// The chip is decoration, not a gate — the Launch button remains the
// authoritative validation surface, so we silently hide on any failure
// mode rather than surface an error.
export default function CostPreviewChip({ filePath, source }: Props) {
  const enabled = (source ?? "").length > 0 || (filePath ?? "").length > 0;
  const { data, isLoading, error } = useQuery({
    queryKey: ["preview-cost", filePath, digest(source)],
    queryFn: () => previewRunCost({ file_path: filePath, source }),
    enabled,
    staleTime: 30_000,
    retry: false,
  });

  if (isLoading || error || !data) return null;
  // `nodes` is null (not []) when the workflow has no LLM node — the
  // server answers {"nodes": null, "notes": ["no_llm_nodes"]}.
  if (!data.nodes || data.nodes.length === 0) return null;
  if (data.notes?.includes("no_pricing_data")) return null;

  const range =
    data.cost_min_usd > 0
      ? `${formatCost(data.cost_min_usd)}–${formatCost(data.cost_max_usd)}`
      : formatCost(data.cost_max_usd);
  const tokens = formatTokens(Math.round((data.tokens_min + data.tokens_max) / 2));

  const tooltip = (
    <div className="space-y-1">
      <div className="font-medium text-fg-default">Estimated run cost</div>
      <div className="text-micro text-fg-muted">
        Range covers retries + plausible second-pass loops. Pricing is best-effort
        — actual cost depends on real token usage.
      </div>
      <ul className="space-y-0.5 mt-1.5">
        {data.nodes.map((n) => (
          <li key={n.node_id} className="flex items-baseline gap-2 text-micro">
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
              {n.cost_min_usd > 0 ? formatCost(n.cost_min_usd) : "—"}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );

  return (
    <Tooltip content={tooltip} side="top">
      <span
        className="inline-flex items-center text-caption px-1.5 py-0.5 rounded border bg-surface-2 text-fg-muted border-border-default tabular-nums cursor-help"
        aria-label={`Estimated cost ${range} for approximately ${tokens} tokens`}
      >
        ≈ {range} · {tokens} tok
      </span>
    </Tooltip>
  );
}
