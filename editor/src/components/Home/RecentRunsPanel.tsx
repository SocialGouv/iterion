import { Link, useLocation } from "wouter";

import { Badge } from "@/components/ui/Badge";
import type { RunSummary } from "@/api/runs";
import { formatRelative } from "@/lib/format";
import {
  STATUS_VARIANT,
  labelForStatus,
  isActiveStatus,
} from "@/components/Runs/runStatusMeta";

interface Props {
  runs: RunSummary[];
  loading: boolean;
  error: string | null;
}

const MAX_RECENT_RUNS = 10;

export default function RecentRunsPanel({ runs, loading, error }: Props) {
  const [, setLocation] = useLocation();
  const recent = runs
    .filter((r) => !isActiveStatus(r.status))
    .slice(0, MAX_RECENT_RUNS);

  return (
    <section className="flex flex-col bg-surface-1 border border-border-default rounded-lg overflow-hidden">
      <header className="px-4 py-2.5 border-b border-border-default flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
          Recent runs
        </h2>
        <Link
          href="/runs"
          className="text-xs text-fg-muted hover:text-fg-default"
        >
          View all →
        </Link>
      </header>

      <div className="flex-1 overflow-auto">
        {loading && runs.length === 0 ? (
          <div className="p-4 text-xs text-fg-subtle">Loading…</div>
        ) : error ? (
          <div className="p-4 text-xs text-danger">{error}</div>
        ) : recent.length === 0 ? (
          <div className="p-4 text-xs text-fg-subtle">
            No runs yet — launch one from the editor.
          </div>
        ) : (
          <ul className="divide-y divide-border-default">
            {recent.map((r) => (
              <li key={r.id}>
                <button
                  onClick={() =>
                    setLocation(`/runs/${encodeURIComponent(r.id)}`)
                  }
                  className="w-full px-4 py-2 flex items-center gap-3 hover:bg-surface-2 text-left"
                >
                  <div className="min-w-0 flex-1">
                    <div className="text-xs font-medium truncate">
                      {r.name || r.workflow_name}
                    </div>
                    {r.name && r.workflow_name !== r.name && (
                      <div className="text-[10px] text-fg-subtle truncate">
                        {r.workflow_name}
                      </div>
                    )}
                  </div>
                  <Badge variant={STATUS_VARIANT[r.status]}>
                    {labelForStatus(r.status)}
                  </Badge>
                  <span className="text-[10px] text-fg-subtle shrink-0 w-16 text-right">
                    {formatRelative(r.updated_at)}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}
