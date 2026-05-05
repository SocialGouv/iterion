import { useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";

import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { listRuns, type RunStatus, type RunSummary } from "@/api/runs";
import { formatRelative } from "@/lib/format";

const POLL_INTERVAL_FAST_MS = 3000;
const POLL_INTERVAL_SLOW_MS = 8000;
// Threshold above which we slow polling to relieve the cloud server.
// Plan §F (T-13) calls this out as the editor's only backpressure
// signal — runners scale via NATS lag, not via WS connection count.
const QUEUED_BACKOFF_THRESHOLD = 10;

// computePollingInterval picks the list polling cadence based on the
// queue depth visible in the most recent fetch. Exported (and pure) so
// the unit test can lock the contract independently of the React tree.
export function computePollingInterval(
  counts: Partial<Record<RunStatus, number>>,
): number {
  const queued = counts.queued ?? 0;
  return queued >= QUEUED_BACKOFF_THRESHOLD
    ? POLL_INTERVAL_SLOW_MS
    : POLL_INTERVAL_FAST_MS;
}

const STATUS_FILTERS: Array<{ value: RunStatus | ""; label: string }> = [
  { value: "", label: "All" },
  { value: "running", label: "Running" },
  // queued sits between running and paused so the eye walks the
  // progression naturally (cloud-ready plan §F T-13).
  { value: "queued", label: "Queued" },
  { value: "paused_waiting_human", label: "Paused" },
  { value: "finished", label: "Finished" },
  { value: "failed", label: "Failed" },
  { value: "failed_resumable", label: "Failed (resumable)" },
  { value: "cancelled", label: "Cancelled" },
];

const STATUS_VARIANT: Record<RunStatus, BadgeVariant> = {
  running: "info",
  paused_waiting_human: "warning",
  finished: "success",
  failed: "danger",
  failed_resumable: "danger",
  cancelled: "neutral",
  queued: "neutral",
};

export default function RunListView() {
  const [, setLocation] = useLocation();
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [status, setStatus] = useState<RunStatus | "">("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const counts = useMemo(() => {
    const m: Partial<Record<RunStatus, number>> = {};
    for (const r of runs) m[r.status] = (m[r.status] ?? 0) + 1;
    return m;
  }, [runs]);

  // Polling cadence backs off when the queue is deep so we don't
  // hammer the cloud server with list requests during a backlog. The
  // useMemo on `counts` is the single source of truth for histograms.
  const pollMs = computePollingInterval(counts);

  useEffect(() => {
    let cancelled = false;
    const fetchRuns = async () => {
      try {
        const out = await listRuns({ status: status || undefined });
        if (!cancelled) {
          setRuns(out);
          setError(null);
          setLoading(false);
        }
      } catch (e) {
        if (!cancelled) {
          setError((e as Error).message);
          setLoading(false);
        }
      }
    };
    fetchRuns();
    const id = setInterval(fetchRuns, pollMs);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [status, pollMs]);

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-1 text-fg-default">
      <header className="border-b border-border-default px-4 py-3 flex items-center gap-3">
        <h1 className="text-sm font-bold">Runs</h1>
        <span className="text-xs text-fg-subtle">{runs.length} total</span>
        <button
          className="ml-auto text-xs px-2 py-1 rounded bg-surface-2 hover:bg-surface-3 text-fg-default"
          onClick={() => setLocation("/edit")}
        >
          Back to editor
        </button>
      </header>

      <div className="px-4 py-2 flex flex-wrap items-center gap-1.5 border-b border-border-default">
        {STATUS_FILTERS.map((f) => {
          const active = status === f.value;
          const count =
            f.value === ""
              ? runs.length
              : counts[f.value as RunStatus] ?? 0;
          return (
            <button
              key={f.value || "all"}
              type="button"
              onClick={() => setStatus(f.value)}
              className={`inline-flex items-center gap-1 rounded-md border text-xs h-6 px-2 ${
                active
                  ? "border-accent/40 bg-accent-soft text-fg-default"
                  : "border-border-default bg-surface-2 text-fg-default hover:bg-surface-3"
              }`}
            >
              {f.label}
              {count > 0 && (
                <span className="text-fg-subtle">{count}</span>
              )}
            </button>
          );
        })}
      </div>

      <div className="flex-1 overflow-auto">
        {loading && runs.length === 0 ? (
          <div className="p-6 text-xs text-fg-subtle">Loading…</div>
        ) : error ? (
          <div className="p-6 text-xs text-danger">{error}</div>
        ) : runs.length === 0 ? (
          <div className="p-6 text-xs text-fg-subtle">
            No runs yet. Launch one from the editor.
          </div>
        ) : (
          <table className="w-full text-xs">
            <thead className="text-fg-subtle">
              <tr className="border-b border-border-default">
                <th className="text-left px-4 py-2 font-medium">Workflow</th>
                <th className="text-left px-4 py-2 font-medium">Status</th>
                <th className="text-left px-4 py-2 font-medium">Started</th>
                <th className="text-left px-4 py-2 font-medium">Duration</th>
                <th className="text-left px-4 py-2 font-medium">Run ID</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr
                  key={r.id}
                  className="border-b border-border-default hover:bg-surface-2 cursor-pointer"
                  onClick={() => setLocation(`/runs/${encodeURIComponent(r.id)}`)}
                >
                  <td className="px-4 py-2">
                    <div className="font-medium">
                      {r.name || r.workflow_name}
                    </div>
                    {(r.name || r.file_path) && (
                      <div className="text-fg-subtle text-[10px] truncate max-w-md">
                        {[r.name && r.workflow_name, r.file_path]
                          .filter(Boolean)
                          .join(" · ")}
                      </div>
                    )}
                  </td>
                  <td className="px-4 py-2">
                    <Badge variant={STATUS_VARIANT[r.status]}>
                      {labelForStatus(r.status)}
                    </Badge>
                    {r.active && (
                      <span
                        className="ml-1.5 inline-block w-1.5 h-1.5 rounded-full bg-info animate-pulse"
                        title="Active in this process"
                      />
                    )}
                  </td>
                  <td className="px-4 py-2 text-fg-muted">
                    {formatRelative(r.created_at)}
                  </td>
                  <td className="px-4 py-2 text-fg-muted">
                    {formatDuration(r.created_at, r.finished_at)}
                  </td>
                  <td className="px-4 py-2 font-mono text-[10px] text-fg-subtle">
                    {r.id}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

function labelForStatus(s: RunStatus): string {
  switch (s) {
    case "paused_waiting_human":
      return "Paused";
    case "failed_resumable":
      return "Failed (resumable)";
    case "queued":
      return "Queued";
    default:
      return s;
  }
}

function formatDuration(startISO: string, endISO?: string): string {
  const start = Date.parse(startISO);
  if (Number.isNaN(start)) return "";
  const end = endISO ? Date.parse(endISO) : Date.now();
  if (Number.isNaN(end)) return "";
  const ms = Math.max(0, end - start);
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const remSec = seconds % 60;
  if (minutes < 60) return `${minutes}m ${remSec}s`;
  const hours = Math.floor(minutes / 60);
  const remMin = minutes % 60;
  return `${hours}h ${remMin}m`;
}
