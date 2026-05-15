import { Link, useLocation } from "wouter";

import { Badge } from "@/components/ui/Badge";
import type { GlobalActiveRun, RunSummary } from "@/api/runs";
import { formatRelative } from "@/lib/format";
import {
  STATUS_VARIANT,
  labelForStatus,
  isActiveStatus,
} from "@/components/Runs/runStatusMeta";
import { useGlobalActiveRuns } from "@/hooks/useGlobalActiveRuns";
import { desktop, isDesktop } from "@/lib/desktopBridge";

interface Props {
  runs: RunSummary[];
  loading: boolean;
  error: string | null;
}

const MAX_RECENT_RUNS = 10;

// RunsPanel renders both the current project's runs AND the runs
// currently RUNNING in other iterion stores on the host (other
// projects, the global ~/.iterion slot). The two sections live in
// the same box, separated by a horizontal rule + a small heading,
// so the operator sees everything in-flight at a glance without
// hunting across two boxes.
//
// Filtering on the home is intentionally tighter than on the
// /runs page: we surface ONLY status === "running" from other
// locations (not queued, not failed, not paused). The "View all →"
// link is the place where the full cross-project history shows up.
export default function RunsPanel({ runs, loading, error }: Props) {
  const [, setLocation] = useLocation();
  const active = runs.filter((r) => isActiveStatus(r.status));
  const recent = runs
    .filter((r) => !isActiveStatus(r.status))
    .slice(0, MAX_RECENT_RUNS);

  const { runs: globalRuns, error: globalError } = useGlobalActiveRuns();
  // Restrict the in-box "other locations" list to truly RUNNING
  // runs only. Queued runs (waiting on the cloud NATS queue) and
  // paused/failed/finished are not surfaced here — they live under
  // the View all link.
  const otherRunning = globalRuns.filter((r) => r.status === "running");

  if (globalError && typeof console !== "undefined") {
    console.warn("RunsPanel: listGlobalActiveRuns failed:", globalError);
  }

  const goToRun = (id: string) =>
    setLocation(`/runs/${encodeURIComponent(id)}`);

  const hasAnything =
    active.length > 0 || recent.length > 0 || otherRunning.length > 0;
  const totalActive = active.length + otherRunning.length;

  return (
    <section className="flex flex-col bg-surface-1 border border-border-default rounded-lg overflow-hidden">
      <header className="px-4 py-2.5 border-b border-border-default flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
          Runs
          {totalActive > 0 && (
            <span className="ml-2 inline-flex items-center gap-1 normal-case tracking-normal text-info-fg">
              <span className="inline-block w-1.5 h-1.5 rounded-full bg-info animate-pulse" />
              {totalActive} active
            </span>
          )}
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
        ) : !hasAnything ? (
          <div className="p-4 text-xs text-fg-subtle">
            No runs yet — launch one from the editor.
          </div>
        ) : (
          <ul className="divide-y divide-border-default">
            {active.map((r) => (
              <li key={r.id}>
                <button
                  onClick={() => goToRun(r.id)}
                  className="w-full px-4 py-2.5 flex items-center gap-3 text-left bg-info-soft/30 hover:bg-info-soft/50 border-l-2 border-info"
                >
                  <span
                    className="inline-block w-2 h-2 rounded-full bg-info animate-pulse shrink-0"
                    aria-hidden="true"
                  />
                  <div className="min-w-0 flex-1">
                    <div className="text-xs font-semibold truncate">
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
                    {r.status === "queued" && r.queue_position
                      ? ` · #${r.queue_position}`
                      : ""}
                  </Badge>
                  <span className="text-[10px] text-fg-subtle shrink-0 w-16 text-right">
                    {formatRelative(r.updated_at)}
                  </span>
                </button>
              </li>
            ))}
            {recent.map((r) => (
              <li key={r.id}>
                <button
                  onClick={() => goToRun(r.id)}
                  className="w-full px-4 py-2 flex items-center gap-3 hover:bg-surface-2 text-left"
                >
                  <div className="min-w-0 flex-1 pl-4">
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
            {otherRunning.length > 0 && (
              <li
                aria-hidden="true"
                className="px-4 py-1.5 bg-surface-2 text-[10px] font-semibold uppercase tracking-wider text-fg-subtle"
              >
                In other locations
              </li>
            )}
            {otherRunning.map((r) => (
              <li key={`${r.store_path}:${r.id}`}>
                <GlobalRunRow run={r} />
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

// openRunCrossDaemon resolves the daemon URL serving the given run's
// store and navigates to its /runs/<id> route. Empty URL → same daemon
// (graceful fallback for the global slot served by the current daemon
// itself). Errors fall back to a same-daemon navigation so the worst
// case is the historical 404, not a swallowed click.
async function openRunCrossDaemon(run: GlobalActiveRun): Promise<void> {
  const target = `/runs/${encodeURIComponent(run.id)}`;
  try {
    const daemonURL = await desktop.getDaemonURLForStore(run.store_path);
    if (daemonURL) {
      window.location.replace(daemonURL.replace(/\/$/, "") + target);
      return;
    }
  } catch (err) {
    if (typeof console !== "undefined") {
      console.warn("openRunCrossDaemon: GetDaemonURLForStore failed:", err);
    }
  }
  // Fallback: current daemon, relative.
  window.location.assign(target);
}

function GlobalRunRow({ run }: { run: GlobalActiveRun }) {
  const variant = STATUS_VARIANT[run.status] ?? "info";
  const label = labelForStatus(run.status);
  const location =
    run.workspace_dir ||
    // Strip the user's home so the path stays readable.
    run.store_path.replace(/^\/home\/[^/]+/, "~");

  // In desktop mode we resolve the correct daemon for this store (a
  // per-project daemon for project slots, the current daemon for the
  // global slot or any unrecognised path) and navigate via
  // window.location so the SPA re-bootstraps against the right
  // backend. In browser / non-Wails mode we fall back to a same-
  // daemon wouter link.
  const inner = (
    <>
      <span
        className="inline-block w-2 h-2 rounded-full bg-info animate-pulse shrink-0"
        aria-hidden="true"
      />
      <div className="min-w-0 flex-1">
        <div className="text-xs font-semibold truncate">
          {run.name || run.workflow_name}
        </div>
        <div className="text-[10px] text-fg-subtle truncate">
          {run.workflow_name}
          {" · "}
          <span title={run.store_path}>{location}</span>
        </div>
      </div>
      <Badge variant={variant} className="shrink-0">
        {label}
      </Badge>
      <span className="text-[10px] text-fg-subtle shrink-0 w-16 text-right">
        {formatRelative(run.updated_at)}
      </span>
    </>
  );

  if (isDesktop()) {
    return (
      <button
        type="button"
        onClick={() => void openRunCrossDaemon(run)}
        className="w-full px-4 py-2.5 flex items-center gap-3 text-left bg-info-soft/20 hover:bg-info-soft/40 border-l-2 border-info"
      >
        {inner}
      </button>
    );
  }
  return (
    <Link
      href={`/runs/${encodeURIComponent(run.id)}`}
      className="w-full px-4 py-2.5 flex items-center gap-3 bg-info-soft/20 hover:bg-info-soft/40 border-l-2 border-info"
    >
      {inner}
    </Link>
  );
}
