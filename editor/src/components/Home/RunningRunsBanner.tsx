import { useLocation } from "wouter";

import { Badge } from "@/components/ui/Badge";
import type { RunSummary } from "@/api/runs";
import {
  STATUS_VARIANT,
  labelForStatus,
  isActiveStatus,
} from "@/components/Runs/runStatusMeta";

interface Props {
  runs: RunSummary[];
}

// Top-of-home banner listing runs that are still moving (running or
// queued). Renders nothing when nothing is active so the home layout
// collapses cleanly.
export default function RunningRunsBanner({ runs }: Props) {
  const [, setLocation] = useLocation();
  const active = runs.filter((r) => isActiveStatus(r.status));
  if (active.length === 0) return null;

  return (
    <div className="border-b border-info/40 bg-info-soft/40 px-4 py-2.5 flex items-center gap-3 overflow-x-auto">
      <span className="text-xs font-medium text-fg-default flex items-center gap-1.5 shrink-0">
        <span className="inline-block w-1.5 h-1.5 rounded-full bg-info animate-pulse" />
        {active.length} run{active.length > 1 ? "s" : ""} active
      </span>
      <div className="flex items-center gap-1.5 flex-wrap">
        {active.map((r) => (
          <button
            key={r.id}
            onClick={() => setLocation(`/runs/${encodeURIComponent(r.id)}`)}
            className="inline-flex items-center gap-1.5 rounded-md border border-border-default bg-surface-1 hover:bg-surface-2 px-2 py-1 text-xs"
            title={r.id}
          >
            <span className="font-medium truncate max-w-[16rem]">
              {r.name || r.workflow_name}
            </span>
            <Badge variant={STATUS_VARIANT[r.status]}>
              {labelForStatus(r.status)}
              {r.status === "queued" && r.queue_position
                ? ` · #${r.queue_position}`
                : ""}
            </Badge>
          </button>
        ))}
      </div>
    </div>
  );
}
