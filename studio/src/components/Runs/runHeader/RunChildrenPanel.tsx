// Extracted from RunHeader.tsx to keep that file focused.
// Run-tree children panel (T4b, refs #125): the DOWN edge companion to
// ForkedFromRow's UP breadcrumb. When the open run spawned shard/fork
// children (parent_run_id == this run on the reverse query), render a
// compact list — one row per child with its shard label, a source badge
// (reusing runSourceMeta), a status badge, and a link to /runs/<id>.
//
// Lazy + self-hiding: fetches children once for the open run (single
// fetch, never N+1 — the run-list callers gate the hook themselves) and
// renders nothing until there is at least one child, so a top-level or
// leaf run shows no panel.

import { Link } from "wouter";

import type { RunHeader as RunHeaderType, RunSummary } from "@/api/runs";
import { Badge } from "@/components/ui/Badge";
import { useRunChildren } from "@/hooks/useRunChildren";
import { STATUS_VARIANT, labelForStatus } from "../runStatusMeta";
import { metaForSource, runSourceKind } from "../runSourceMeta";

// childLabel derives the human-friendly slot label for a child run:
// the explicit shard_label when set, else "#<n>/<count>" from the shard
// tuple (shard_index is 0-based and omitted when 0, so treat absent as
// 0), else the run's own name / id prefix for a plain fork child.
export function childLabel(run: RunSummary): string {
  if (run.shard_label) return run.shard_label;
  if (run.shard_count) return `#${(run.shard_index ?? 0) + 1}/${run.shard_count}`;
  return run.name || run.id.slice(0, 12);
}

function ChildRow({ child }: { child: RunSummary }) {
  const source = metaForSource(runSourceKind(child));
  const SourceIcon = source.Icon;
  return (
    <li className="flex items-center gap-2">
      <Badge variant={source.variant} title={source.description}>
        <SourceIcon className="h-3 w-3" aria-hidden />
        {source.label}
      </Badge>
      <Link
        href={`/runs/${encodeURIComponent(child.id)}`}
        className="font-mono text-fg-default hover:text-info underline-offset-2 hover:underline"
        title={`Open child run ${child.id}`}
      >
        {childLabel(child)}
      </Link>
      <Badge variant={STATUS_VARIANT[child.status]} className="ml-auto">
        {labelForStatus(child.status)}
      </Badge>
    </li>
  );
}

// RunChildrenPanel renders the open run's shard/child subtree. Mounted
// unconditionally next to ForkedFromRow — it self-hides while the fetch
// is in flight or when the run has no children, so the caller needn't
// know the tree shape up front.
export default function RunChildrenPanel({ run }: { run: RunHeaderType }) {
  const { data: children } = useRunChildren(run.id);
  if (children.length === 0) return null;
  return (
    <div className="shrink-0 border-b border-info/30 bg-info-soft/40 px-4 py-1.5">
      <div className="mb-1 text-micro uppercase tracking-wide text-fg-subtle">
        ⑃ Children ({children.length})
      </div>
      <ul className="space-y-1 text-micro">
        {children.map((c) => (
          <ChildRow key={c.id} child={c} />
        ))}
      </ul>
    </div>
  );
}
