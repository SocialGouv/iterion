import { Link } from "wouter";

import { Badge } from "@/components/ui/Badge";
import { formatRelative } from "@/lib/format";
import {
  STATUS_VARIANT,
  labelForStatus,
} from "@/components/Runs/runStatusMeta";
import { useGlobalActiveRuns } from "@/hooks/useGlobalActiveRuns";
import type { GlobalActiveRun } from "@/api/runs";
import { desktop, isDesktop } from "@/lib/desktopBridge";

// GlobalActiveRunsBanner surfaces active runs from EVERY iterion
// store on the host — the global ~/.iterion slot plus every per-
// project slot under ~/.iterion/projects/. The point: an operator
// who hasn't selected a project (Home view) still sees in-flight
// work, and one inside project A sees the run they kicked off in
// project B without having to switch.
//
// Renders nothing when the list is empty so it stays out of the way
// during normal browsing. The store path is shown verbatim so the
// operator can copy-paste it; we don't surface a "switch project"
// action yet because each project routes through its own server
// process (the active runs may be served by a different daemon).
export default function GlobalActiveRunsBanner() {
  const { runs, error } = useGlobalActiveRuns();

  if (error) {
    // Silent fail: a broken cross-store scan should NOT block the
    // rest of the Home view from rendering. Surfaced in devtools.
    if (typeof console !== "undefined") {
      console.warn("GlobalActiveRunsBanner: listGlobalActiveRuns failed:", error);
    }
    return null;
  }
  if (runs.length === 0) {
    return null;
  }

  return (
    <section
      aria-label="Active runs across all iterion stores"
      className="md:col-span-2 bg-info-soft/30 border border-info/40 rounded-lg overflow-hidden"
    >
      <header className="px-4 py-2.5 border-b border-info/40 flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-info-fg flex items-center gap-2">
          <span
            className="inline-block w-1.5 h-1.5 rounded-full bg-info animate-pulse"
            aria-hidden="true"
          />
          {runs.length} active run{runs.length === 1 ? "" : "s"} in other locations
        </h2>
      </header>
      <ul className="divide-y divide-info/30">
        {runs.map((r) => (
          <GlobalRunRow key={`${r.store_path}:${r.id}`} run={r} />
        ))}
      </ul>
    </section>
  );
}

interface RowProps {
  run: GlobalActiveRun;
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

function GlobalRunRow({ run }: RowProps) {
  const variant = STATUS_VARIANT[run.status] ?? "info";
  const label = labelForStatus(run.status);
  const location =
    run.workspace_dir ||
    // Strip the user's home so the path stays readable.
    run.store_path.replace(/^\/home\/[^/]+/, "~");

  return (
    <li className="px-4 py-2.5 flex items-center gap-3 hover:bg-info-soft/40">
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
      <span className="text-[10px] text-fg-subtle shrink-0">
        {formatRelative(run.updated_at)}
      </span>
      {/* In desktop mode we resolve the correct daemon for this store
          (a per-project daemon for project slots, the current daemon
          for the global slot or any unrecognised path) and navigate via
          window.location so the SPA re-bootstraps against the right
          backend. In browser / non-Wails mode we fall back to the
          existing wouter same-daemon link. */}
      {isDesktop() ? (
        <button
          type="button"
          onClick={() => void openRunCrossDaemon(run)}
          className="text-xs text-info-fg hover:underline shrink-0"
        >
          Open →
        </button>
      ) : (
        <Link
          href={`/runs/${encodeURIComponent(run.id)}`}
          className="text-xs text-info-fg hover:underline shrink-0"
        >
          Open →
        </Link>
      )}
    </li>
  );
}
