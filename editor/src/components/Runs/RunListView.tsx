import { useState } from "react";
import { useLocation } from "wouter";

import { Badge } from "@/components/ui/Badge";
import { type RunStatus } from "@/api/runs";
import { formatRelative } from "@/lib/format";
import ProjectLabel from "@/components/shared/ProjectLabel";
import NavLinks from "@/components/shared/NavLinks";
import { useRuns } from "@/hooks/useRuns";
import { STATUS_VARIANT, labelForStatus } from "./runStatusMeta";
import QueueDepthBar from "./QueueDepthBar";

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

export default function RunListView() {
  const [, setLocation] = useLocation();
  const [status, setStatus] = useState<RunStatus | "">("");
  const { runs, counts, loading, error } = useRuns({ status });

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-1 text-fg-default">
      <header className="border-b border-border-default px-4 py-2.5 flex items-center gap-3">
        <NavLinks active="runs" />
        <ProjectLabel />
        <span className="text-xs text-fg-subtle">{runs.length} total</span>
      </header>

      <QueueDepthBar counts={counts} />

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
