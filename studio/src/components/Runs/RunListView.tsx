import { memo, useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { useLocation, useSearch } from "wouter";

import {
  Cross2Icon,
  MagnifyingGlassIcon,
  ReloadIcon,
  RocketIcon,
} from "@radix-ui/react-icons";

import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { IconButton } from "@/components/ui/IconButton";
import { Input } from "@/components/ui/Input";
import { LiveDot } from "@/components/ui/LiveDot";
import type { RunStatus, RunSummary } from "@/api/runs";
import { formatRelative } from "@/lib/format";
import { useDebounce } from "@/hooks/useDebounce";
import { useRuns } from "@/hooks/useRuns";
import { STATUS_VARIANT, labelForStatus } from "./runStatusMeta";
import QueueDepthBar from "./QueueDepthBar";
import {
  filterRuns,
  parseSince,
  SINCE_FILTERS,
  type SinceFilter,
} from "./runListFilter";

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

const QUERY_DEBOUNCE_MS = 150;

export default function RunListView() {
  const [location, setLocation] = useLocation();
  const search = useSearch();
  // Initial state derives from the URL so reload + browser-back
  // restore the user's filters. After mount we drive the URL from
  // state, never the reverse.
  const initial = useMemo(() => {
    const p = new URLSearchParams(search);
    return {
      query: p.get("q") ?? "",
      since: parseSince(p.get("since")),
    };
    // Mount-only: subsequent search-string changes come from our own
    // setLocation calls below and must not clobber in-flight state.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const [status, setStatus] = useState<RunStatus | "">("");
  const [queryInput, setQueryInput] = useState(initial.query);
  const query = useDebounce(queryInput, QUERY_DEBOUNCE_MS);
  const [since, setSince] = useState<SinceFilter>(initial.since);

  // Mirror committed (debounced) filters into the URL with replace
  // semantics — keeps the back-button stack uncluttered while still
  // making the page bookmarkable. Status is intentionally not part
  // of the URL contract yet (kept consistent with prior behaviour).
  useEffect(() => {
    const p = new URLSearchParams();
    if (query) p.set("q", query);
    if (since !== "all") p.set("since", since);
    const qs = p.toString();
    const target = qs ? `/runs?${qs}` : "/runs";
    const current = search ? `${location}?${search}` : location;
    if (target !== current) setLocation(target, { replace: true });
  }, [query, since, setLocation, location, search]);

  const { runs, counts, loading, error } = useRuns({ status });

  const filteredRuns = useMemo(
    () => filterRuns(runs, { query, since }),
    [runs, query, since],
  );

  const openRun = useCallback(
    (id: string) => setLocation(`/runs/${encodeURIComponent(id)}`),
    [setLocation],
  );

  // Force a re-render once per second while at least one visible run
  // is still in-flight (no finished_at), so the duration column ticks
  // forward instead of freezing on whatever value the last poll
  // produced. Idle when every visible run has finished.
  const hasLiveRun = useMemo(
    () => filteredRuns.some((r) => !r.finished_at),
    [filteredRuns],
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

  const filtersActive = query !== "" || since !== "all";
  const clearFilters = useCallback(() => {
    setQueryInput("");
    setSince("all");
  }, []);

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-1 text-fg-default">
      <QueueDepthBar counts={counts} />

      <div className="px-4 py-2 flex flex-col gap-2 border-b border-border-default">
        <div className="flex items-center gap-2">
          <div className="flex-1 max-w-md">
            <Input
              type="search"
              value={queryInput}
              onChange={(e) => setQueryInput(e.currentTarget.value)}
              placeholder="Search name, workflow, file path, run id…"
              leadingIcon={<MagnifyingGlassIcon />}
              aria-label="Search runs"
            />
          </div>
          <div className="flex items-center gap-1.5">
            {SINCE_FILTERS.map((f) => (
              <FilterChip
                key={f.value}
                active={since === f.value}
                label={f.label}
                onClick={() => setSince(f.value)}
              />
            ))}
          </div>
          {filtersActive && (
            <IconButton
              label="Clear filters"
              size="sm"
              variant="ghost"
              onClick={clearFilters}
            >
              <Cross2Icon />
            </IconButton>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          {STATUS_FILTERS.map((f) => {
            const count =
              f.value === ""
                ? runs.length
                : counts[f.value as RunStatus] ?? 0;
            return (
              <FilterChip
                key={f.value || "all"}
                active={status === f.value}
                label={f.label}
                count={count > 0 ? count : undefined}
                size="sm"
                onClick={() => setStatus(f.value)}
              />
            );
          })}
        </div>
      </div>

      <div className="flex-1 overflow-auto">
        {loading && runs.length === 0 ? (
          <EmptyState
            message="Loading runs…"
            icon={<ReloadIcon className="animate-spin" />}
          />
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
        ) : filteredRuns.length === 0 ? (
          status !== "" ? (
            <EmptyState
              title={`No ${statusFilterLabel(status)} runs`}
              message="Pick a different status, or clear the filters to see everything."
              action={
                <Button variant="secondary" size="sm" onClick={() => setStatus("")}>
                  Show all statuses
                </Button>
              }
              secondaryAction={
                filtersActive ? (
                  <Button variant="ghost" size="sm" onClick={clearFilters}>
                    Clear search / date
                  </Button>
                ) : undefined
              }
            />
          ) : (
            <EmptyState
              title="No matching runs"
              message="Try a different search term or widen the date range."
              action={
                <Button variant="secondary" size="sm" onClick={clearFilters}>
                  Clear filters
                </Button>
              }
            />
          )
        ) : (
          <>
            {/* Desktop / tablet: standard 6-column table. */}
            <table className="w-full text-xs hidden sm:table">
              <thead className="text-fg-subtle">
                <tr className="border-b border-border-default">
                  <th className="text-left px-4 py-2 font-medium">Run</th>
                  <th className="text-left px-4 py-2 font-medium">Workflow</th>
                  <th className="text-left px-4 py-2 font-medium">Status</th>
                  <th className="text-left px-4 py-2 font-medium">Started</th>
                  <th className="text-left px-4 py-2 font-medium">Duration</th>
                  <th className="text-left px-4 py-2 font-medium">Run ID</th>
                </tr>
              </thead>
              <tbody>
                {filteredRuns.map((r) => (
                  <RunRow key={r.id} run={r} onOpen={openRun} />
                ))}
              </tbody>
            </table>
            <ul className="sm:hidden divide-y divide-border-default">
              {filteredRuns.map((r) => (
                <li key={r.id}>
                  <RunCard run={r} onOpen={openRun} />
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </div>
  );
}

function FilterChip({
  active,
  label,
  count,
  size = "md",
  onClick,
}: {
  active: boolean;
  label: ReactNode;
  count?: number;
  size?: "sm" | "md";
  onClick: () => void;
}) {
  const heightCls = size === "sm" ? "h-6" : "h-7";
  const activeCls = active
    ? "border-accent/40 bg-accent-soft text-fg-default"
    : "border-border-default bg-surface-2 text-fg-default hover:bg-surface-3";
  return (
    <button
      type="button"
      onClick={onClick}
      className={`inline-flex items-center gap-1 rounded-md border text-xs px-2 ${heightCls} ${activeCls}`}
    >
      {label}
      {count !== undefined && <span className="text-fg-subtle">{count}</span>}
    </button>
  );
}

// Memoised so the parent's per-row callback (now stable via useCallback)
// doesn't force every row to re-render when one run mutates.
const RunRow = memo(function RunRow({
  run,
  onOpen,
}: {
  run: RunSummary;
  onOpen: (id: string) => void;
}) {
  return (
    <tr
      className="border-b border-border-default hover:bg-surface-2 cursor-pointer"
      onClick={() => onOpen(run.id)}
    >
      <td className="px-4 py-2">
        <div className="font-medium">{friendlyLabel(run)}</div>
      </td>
      <td className="px-4 py-2">
        {workflowDisplay(run) && (
          <div className="text-fg-default">{workflowDisplay(run)}</div>
        )}
        {run.file_path && (
          <div className="text-fg-subtle text-[10px] truncate max-w-md">
            {run.file_path}
          </div>
        )}
      </td>
      <td className="px-4 py-2">
        <Badge variant={STATUS_VARIANT[run.status]}>
          {labelForStatus(run.status)}
        </Badge>
        {run.active && (
          <LiveDot
            tone="live"
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
      <td className="px-4 py-2 font-mono text-[10px] text-fg-subtle" title={run.id}>
        {shortRunID(run.id)}
      </td>
    </tr>
  );
});

const RunCard = memo(function RunCard({
  run,
  onOpen,
}: {
  run: RunSummary;
  onOpen: (id: string) => void;
}) {
  return (
    <button
      type="button"
      onClick={() => onOpen(run.id)}
      className="w-full text-left px-4 py-3 flex flex-col gap-1 min-h-[44px] hover:bg-surface-2 active:bg-surface-3"
    >
      <div className="flex items-center gap-2 min-w-0">
        <Badge variant={STATUS_VARIANT[run.status]}>
          {labelForStatus(run.status)}
        </Badge>
        {run.active && (
          <LiveDot tone="live" size="sm" label="Active in this process" />
        )}
        <span className="font-medium truncate">
          {friendlyLabel(run)}
        </span>
      </div>
      {workflowDisplay(run) && (
        <div className="text-[11px] text-fg-default truncate">
          {workflowDisplay(run)}
        </div>
      )}
      <div className="text-[11px] text-fg-muted flex flex-wrap gap-x-2">
        <span>{formatRelative(run.created_at)}</span>
        <span>·</span>
        <span>{formatDuration(run.created_at, run.finished_at)}</span>
      </div>
      <div
        className="text-[10px] text-fg-subtle font-mono truncate"
        title={run.id}
      >
        {shortRunID(run.id)}
      </div>
    </button>
  );
});

// hasFriendlyName returns true when run.name is set AND differs from
// run.id. Defensive guard against historical bugs where dispatcher-
// spawned runs aliased Name to the composite RunID (now fixed — see
// pkg/dispatcher/loop.go); legacy stores may still contain such rows.
function hasFriendlyName(run: RunSummary): boolean {
  return Boolean(run.name) && run.name !== run.id;
}

// friendlyLabel returns the per-run instance label for the "Run"
// column. Falls back to workflow_name for legacy runs (persisted
// before the friendly-name feature shipped).
function friendlyLabel(run: RunSummary): string {
  return hasFriendlyName(run) ? run.name! : run.workflow_name;
}

// workflowDisplay returns the label for the "Workflow" column. Returns
// "" when the value would duplicate the run id (dispatcher-spawned
// runs in some legacy paths aliased workflow_name to the composite
// run id; suppress to keep the row from showing the id twice).
function workflowDisplay(run: RunSummary): string {
  const name = run.bundle_name || run.workflow_name;
  if (!name || name === run.id) return "";
  return name;
}

// shortRunID collapses a long run id to a glanceable prefix. Keeps
// the tooltip-attached full id within reach via `title=`. For the
// dispatcher's composite ids (e.g. `dispatcher-native_<uuid>-<seq>-<ts>`),
// we surface the prefix + the first UUID segment so two siblings
// from the same dispatcher slot are still visually distinct.
function shortRunID(id: string): string {
  if (!id) return "";
  const dispPrefix = "dispatcher-native_";
  if (id.startsWith(dispPrefix)) {
    const tail = id.slice(dispPrefix.length);
    const dash = tail.indexOf("-");
    return `disp:${dash > 0 ? tail.slice(0, dash) : tail.slice(0, 8)}`;
  }
  return id.length > 14 ? id.slice(0, 14) + "…" : id;
}

// statusFilterLabel returns a lower-case fragment suited for the
// "No <status> runs" empty headline (matches the chip label so the
// phrasing stays consistent between the chip and the empty state).
// Exported for the corresponding unit test; not consumed elsewhere.
export function statusFilterLabel(status: RunStatus | ""): string {
  if (status === "") return "matching";
  const entry = STATUS_FILTERS.find((f) => f.value === status);
  const label = entry?.label ?? labelForStatus(status);
  return label.toLowerCase();
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
