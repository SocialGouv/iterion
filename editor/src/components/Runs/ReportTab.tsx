import { useMemo, useState } from "react";

import type { CostBucket } from "@/hooks/useRunReport";
import { useRunReport } from "@/hooks/useRunReport";
import { formatCost, formatTokens } from "@/lib/format";

interface Props {
  // Click handler for a node-bucket row; the parent uses this to drive
  // the canvas + NodeDetailPanel selection so the user can drill from
  // "this node cost the most" straight to the trace.
  onSelectNode?: (nodeId: string) => void;
}

// ReportTab renders three cost breakdowns (provider / model / node) in
// a scrollable panel meant for the bottom tab area. The data source is
// the same node_finished events the rest of the run console reads, so
// the report updates live as the run progresses.
export default function ReportTab({ onSelectNode }: Props) {
  const report = useRunReport();
  if (!report.hasCost) {
    return (
      <div className="h-full flex items-center justify-center px-4 text-fg-subtle text-xs">
        No LLM cost recorded for this run yet — the report fills in as
        nodes finish.
      </div>
    );
  }
  return (
    <div className="h-full overflow-auto px-4 py-3 space-y-4 text-xs">
      <SummaryStrip report={report} />
      <BreakdownSection
        title="By provider"
        hint="Grouped by API-key billing surface (anthropic, openai, …)"
        buckets={report.byProvider}
        total={report.totalCostUsd}
      />
      <BreakdownSection
        title="By model"
        hint="Each node is attributed to its dominant model (cost is per-node, not per-step)"
        buckets={report.byModel}
        total={report.totalCostUsd}
      />
      <BreakdownSection
        title="By node"
        hint="Click a row to jump to the node in the canvas"
        buckets={report.byNode}
        total={report.totalCostUsd}
        onSelect={onSelectNode}
        collapsibleAfter={10}
      />
    </div>
  );
}

function SummaryStrip({
  report,
}: {
  report: ReturnType<typeof useRunReport>;
}) {
  return (
    <div className="flex flex-wrap items-baseline gap-x-6 gap-y-1 pb-2 border-b border-border-default">
      <Stat
        label="total cost"
        value={formatCost(report.totalCostUsd)}
        title={`$${report.totalCostUsd.toFixed(6)}`}
        emphasis
      />
      <Stat
        label="total tokens"
        value={formatTokens(report.totalTokens)}
        title={report.totalTokens.toLocaleString()}
      />
      <Stat label="providers" value={String(report.byProvider.length)} />
      <Stat label="models" value={String(report.byModel.length)} />
      <Stat label="nodes" value={String(report.byNode.length)} />
    </div>
  );
}

function Stat({
  label,
  value,
  title,
  emphasis,
}: {
  label: string;
  value: string;
  title?: string;
  emphasis?: boolean;
}) {
  return (
    <span className="inline-flex items-baseline gap-1.5" title={title}>
      <span className="text-fg-subtle">{label}</span>
      <span
        className={`font-mono ${
          emphasis ? "text-base font-semibold text-fg-default" : "font-semibold"
        }`}
      >
        {value}
      </span>
    </span>
  );
}

interface BreakdownProps {
  title: string;
  hint?: string;
  buckets: CostBucket[];
  total: number;
  onSelect?: (key: string) => void;
  // When set, only the first N rows render initially; a "show all" toggle
  // expands the rest. Useful for the by-node breakdown which can have
  // dozens of rows on a deep workflow.
  collapsibleAfter?: number;
}

function BreakdownSection({
  title,
  hint,
  buckets,
  total,
  onSelect,
  collapsibleAfter,
}: BreakdownProps) {
  const [expanded, setExpanded] = useState(false);
  const max = useMemo(
    () => buckets.reduce((m, b) => Math.max(m, b.costUsd), 0),
    [buckets],
  );
  const collapsedCount =
    collapsibleAfter && buckets.length > collapsibleAfter
      ? collapsibleAfter
      : buckets.length;
  const visible = expanded ? buckets : buckets.slice(0, collapsedCount);
  const hidden = buckets.length - visible.length;

  return (
    <section>
      <header className="flex items-baseline justify-between mb-1.5">
        <h3 className="text-xs font-semibold text-fg-default">{title}</h3>
        {hint && (
          <span className="text-[10px] text-fg-subtle truncate ml-3 max-w-md">
            {hint}
          </span>
        )}
      </header>
      <ul className="space-y-0.5">
        {visible.map((b) => (
          <BucketRow
            key={b.key}
            bucket={b}
            total={total}
            scaleMax={max}
            onSelect={onSelect}
          />
        ))}
      </ul>
      {hidden > 0 && (
        <button
          type="button"
          onClick={() => setExpanded(true)}
          className="mt-1.5 text-[10px] text-fg-subtle hover:text-fg-default"
        >
          show {hidden} more
        </button>
      )}
      {expanded && collapsibleAfter && buckets.length > collapsibleAfter && (
        <button
          type="button"
          onClick={() => setExpanded(false)}
          className="mt-1.5 ml-3 text-[10px] text-fg-subtle hover:text-fg-default"
        >
          collapse
        </button>
      )}
    </section>
  );
}

function BucketRow({
  bucket,
  total,
  scaleMax,
  onSelect,
}: {
  bucket: CostBucket;
  total: number;
  // The largest cost in this section — used to scale the bar to its
  // section, not to a global max. Otherwise small sections (e.g. just
  // 2 providers) would render two near-full-width bars that read as
  // identical even when one is 10× the other.
  scaleMax: number;
  onSelect?: (key: string) => void;
}) {
  const pct = total > 0 ? (bucket.costUsd / total) * 100 : 0;
  const barPct = scaleMax > 0 ? (bucket.costUsd / scaleMax) * 100 : 0;
  const clickable = !!onSelect;
  const Component = clickable ? "button" : "div";

  return (
    <li>
      <Component
        type={clickable ? "button" : undefined}
        onClick={clickable ? () => onSelect!(bucket.key) : undefined}
        className={`w-full flex items-center gap-2 py-1 px-1 rounded ${
          clickable ? "hover:bg-surface-2 cursor-pointer text-left" : ""
        }`}
      >
        <span
          className="font-mono text-fg-default truncate min-w-0 flex-shrink"
          style={{ flexBasis: "10rem" }}
          title={bucket.label}
        >
          {bucket.label}
        </span>
        <div className="flex-1 min-w-0 h-2 bg-surface-2 rounded overflow-hidden">
          <div
            className="h-full bg-info rounded"
            style={{ width: `${barPct}%` }}
          />
        </div>
        <span
          className="font-mono text-fg-default text-right"
          style={{ flexBasis: "5rem" }}
          title={`$${bucket.costUsd.toFixed(6)}`}
        >
          {formatCost(bucket.costUsd)}
        </span>
        <span
          className="text-fg-subtle text-right text-[10px]"
          style={{ flexBasis: "3rem" }}
        >
          {pct.toFixed(0)}%
        </span>
        <span
          className="text-fg-subtle text-right text-[10px] hidden sm:inline-block"
          style={{ flexBasis: "5rem" }}
          title={`${bucket.tokens.toLocaleString()} tokens · ${bucket.count} ${
            bucket.count === 1 ? "exec" : "execs"
          }`}
        >
          {formatTokens(bucket.tokens)}
        </span>
      </Component>
    </li>
  );
}
