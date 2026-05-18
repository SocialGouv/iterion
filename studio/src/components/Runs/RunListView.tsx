import { useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";

import { RocketIcon } from "@radix-ui/react-icons";

import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { LiveDot } from "@/components/ui/LiveDot";
import type { RunStatus, RunSummary } from "@/api/runs";
import { formatRelative } from "@/lib/format";
import AppHeader from "@/components/shared/AppHeader";
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

  // Force a re-render once per second while at least one run is
  // still in-flight (no finished_at), so the duration column ticks
  // forward instead of freezing on whatever value the last poll
  // produced. Idle when every visible run has finished.
  const hasLiveRun = useMemo(
    () => runs.some((r) => !r.finished_at),
    [runs],
  );
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!hasLiveRun) return;
    const id = setInterval(() => setTick((t) => t + 1), 1000);
    return () => clearInterval(id);
  }, [hasLiveRun]);
  // Read tick so React preserves the dependency edge — the explicit
  // void prevents the linter from treating it as dead.
  void tick;

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-1 text-fg-default">
      <AppHeader
        active="runs"
        rightActions={
          <span className="text-xs text-fg-subtle">{runs.length} total</span>
        }
      />

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

      <div id="main-content" tabIndex={-1} className="flex-1 overflow-auto outline-none">
        {loading && runs.length === 0 ? (
          <EmptyState message="Loading…" />
        ) : error ? (
          <EmptyState message={<span className="text-danger">{error}</span>} />
        ) : runs.length === 0 ? (
          <EmptyState
            title="No runs yet"
            message="Launch a workflow from the editor to populate this list."
            action={
              <Button
                variant="primary"
                size="sm"
                leadingIcon={<RocketIcon />}
                onClick={() => setLocation("/editor")}
              >
                Open editor
              </Button>
            }
            secondaryAction={
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setLocation("/")}
              >
                Home
              </Button>
            }
          />
        ) : (
          <>
            {/* Desktop / tablet: standard 5-column table. */}
            <table className="w-full text-xs hidden sm:table">
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
                  <RunRow key={r.id} run={r} onOpen={() => setLocation(`/runs/${encodeURIComponent(r.id)}`)} />
                ))}
              </tbody>
            </table>
            {/* Mobile (< sm): vertical card list. Same data, no
                horizontal scroll; tap-area meets WCAG 2.5.5 (44px). */}
            <ul className="sm:hidden divide-y divide-border-default">
              {runs.map((r) => (
                <li key={r.id}>
                  <RunCard run={r} onOpen={() => setLocation(`/runs/${encodeURIComponent(r.id)}`)} />
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </div>
  );
}

// Table row used at >= sm. Extracted so the mobile card and the table
// share a single source of truth for the displayed fields.
function RunRow({ run, onOpen }: { run: RunSummary; onOpen: () => void }) {
  return (
    <tr
      className="border-b border-border-default hover:bg-surface-2 cursor-pointer"
      onClick={onOpen}
    >
      <td className="px-4 py-2">
        <div className="font-medium">{run.name || run.workflow_name}</div>
        {(run.name || run.file_path) && (
          <div className="text-fg-subtle text-[10px] truncate max-w-md">
            {[run.name && run.workflow_name, run.file_path]
              .filter(Boolean)
              .join(" · ")}
          </div>
        )}
      </td>
      <td className="px-4 py-2">
        <Badge variant={STATUS_VARIANT[run.status]}>
          {labelForStatus(run.status)}
        </Badge>
        {run.active && (
          <LiveDot
            tone="info"
            size="sm"
            className="ml-1.5"
            label="Active in this process"
          />
        )}
      </td>
      <td className="px-4 py-2 text-fg-muted">{formatRelative(run.created_at)}</td>
      <td className="px-4 py-2 text-fg-muted">
        {formatDuration(run.created_at, run.finished_at)}
      </td>
      <td className="px-4 py-2 font-mono text-[10px] text-fg-subtle">
        {run.id}
      </td>
    </tr>
  );
}

// Stacked card variant for < sm. Status + workflow on top line; metadata
// in a compact second row; run id wraps as a third tiny line so phone
// users can copy/share without horizontal scroll.
function RunCard({ run, onOpen }: { run: RunSummary; onOpen: () => void }) {
  return (
    <button
      type="button"
      onClick={onOpen}
      className="w-full text-left px-4 py-3 flex flex-col gap-1 min-h-[44px] hover:bg-surface-2 active:bg-surface-3"
    >
      <div className="flex items-center gap-2 min-w-0">
        <Badge variant={STATUS_VARIANT[run.status]}>
          {labelForStatus(run.status)}
        </Badge>
        {run.active && (
          <LiveDot tone="info" size="sm" label="Active in this process" />
        )}
        <span className="font-medium truncate">
          {run.name || run.workflow_name}
        </span>
      </div>
      <div className="text-[11px] text-fg-muted flex flex-wrap gap-x-2">
        <span>{formatRelative(run.created_at)}</span>
        <span>·</span>
        <span>{formatDuration(run.created_at, run.finished_at)}</span>
      </div>
      <div className="text-[10px] text-fg-subtle font-mono truncate">
        {run.id}
      </div>
    </button>
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
