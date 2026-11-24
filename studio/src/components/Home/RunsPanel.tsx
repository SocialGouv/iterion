import { useState } from "react";
import { Link, useLocation } from "wouter";

import { Badge } from "@/components/ui/Badge";
import { EmptyState } from "@/components/ui/EmptyState";
import { Skeleton } from "@/components/ui/Skeleton";
import { LiveDot } from "@/components/ui/LiveDot";
import { crossDaemonRunStore, globalRunStoreParam, type GlobalActiveRun, type RunSummary } from "@/api/runs";
import { formatRelative } from "@/lib/format";
import { STUDIO_BASE, scopePrefix } from "@/lib/scope";
import {
  STATUS_VARIANT,
  labelForStatus,
  isActiveStatus,
} from "@/components/Runs/runStatusMeta";
import { useGlobalActiveRuns } from "@/hooks/useGlobalActiveRuns";
import { desktop, isDesktop } from "@/lib/desktopBridge";
import { EXPECTED_BIRTH_PARAM, expectedBirthWire, navigationBirthState } from "@/lib/lostRuns";

interface Props {
  runs: RunSummary[];
  loading: boolean;
  error: string | null;
  // Active-repo scope (cloud): full name shown in the header so the
  // operator knows the list is filtered. Null = unscoped.
  scope?: string | null;
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
export default function RunsPanel({ runs, loading, error, scope }: Props) {
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

  // The clicked row's birth travels with the navigation: the view it opens
  // expects that run, whatever a later poll lists under the ID.
  const goToRun = (id: string) =>
    setLocation(`/runs/${encodeURIComponent(id)}`, {
      state: navigationBirthState(id, runs.find((r) => r.id === id)?.birth_id),
    });

  const hasAnything =
    active.length > 0 || recent.length > 0 || otherRunning.length > 0;
  const totalActive = active.length + otherRunning.length;

  return (
    <section className="flex flex-col bg-surface-1 border border-border-default rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
      <header className="px-4 py-2.5 border-b border-border-default flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
          Runs
          {scope && (
            <span className="ml-2 normal-case tracking-normal font-normal font-mono text-fg-subtle">
              {scope}
            </span>
          )}
          {totalActive > 0 && (
            <span className="ml-2 inline-flex items-center gap-1 normal-case tracking-normal text-info-fg">
              <LiveDot tone="info" size="sm" />
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
          <ul className="divide-y divide-border-default" aria-label="Loading runs">
            {Array.from({ length: 4 }).map((_, i) => (
              <li key={i} className="px-4 py-2.5 flex items-center gap-3">
                <Skeleton className="h-2.5 w-2.5 rounded-full shrink-0" />
                <div className="min-w-0 flex-1 space-y-1.5">
                  <Skeleton className="h-3 w-1/2" />
                  <Skeleton className="h-2.5 w-1/3" />
                </div>
              </li>
            ))}
          </ul>
        ) : error ? (
          <EmptyState message={<span className="text-danger">{error}</span>} className="py-6" />
        ) : !hasAnything ? (
          <EmptyState message="No runs yet — launch one from the studio." caret className="py-6" />
        ) : (
          <ul className="divide-y divide-border-default">
            {active.map((r) => (
              <li key={r.id}>
                <button
                  onClick={() => goToRun(r.id)}
                  className="w-full px-4 py-2.5 flex items-center gap-3 text-left bg-info-soft/30 hover:bg-info-soft/50 border-l-2 border-info"
                >
                  <LiveDot tone="info" size="md" className="shrink-0" />
                  <span className="min-w-0 flex-1">
                    <span className="block text-xs font-semibold truncate">
                      {r.name || r.workflow_name}
                    </span>
                    {r.name && r.workflow_name !== r.name && (
                      <span className="block text-caption text-fg-muted truncate">
                        {r.workflow_name}
                      </span>
                    )}
                  </span>
                  <Badge variant={STATUS_VARIANT[r.status]}>
                    {labelForStatus(r.status)}
                    {r.status === "queued" && r.queue_position
                      ? ` · #${r.queue_position}`
                      : ""}
                  </Badge>
                  <span className="text-caption text-fg-muted shrink-0 w-16 text-right">
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
                  <span className="min-w-0 flex-1 pl-4">
                    <span className="block text-xs font-medium truncate">
                      {r.name || r.workflow_name}
                    </span>
                    {r.name && r.workflow_name !== r.name && (
                      <span className="block text-caption text-fg-subtle truncate">
                        {r.workflow_name}
                      </span>
                    )}
                  </span>
                  <Badge variant={STATUS_VARIANT[r.status]}>
                    {labelForStatus(r.status)}
                  </Badge>
                  <span className="text-caption text-fg-subtle shrink-0 w-16 text-right">
                    {formatRelative(r.updated_at)}
                  </span>
                </button>
              </li>
            ))}
            {otherRunning.length > 0 && (
              <li
                aria-hidden="true"
                className="px-4 py-1.5 bg-surface-2 text-caption font-semibold uppercase tracking-wider text-fg-muted"
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

// withRunStore appends `?store=<store>` to a run path; none for "".
function withRunStore(path: string, store: string): string {
  return store ? `${path}?store=${encodeURIComponent(store)}` : path;
}

// fullPageRunPath is a run view path a full page load opens: `?store=` for a
// foreign store (outside the guarantee), else the clicked row's birth as
// `?expected_birth=` — the page registry does not survive the load, so the
// birth rides in the URL and the destination pins it before its first
// request.
export function fullPageRunPath(path: string, store: string, birth: string | undefined): string {
  if (store) return withRunStore(path, store);
  return typeof birth === "string" ? `${path}?${EXPECTED_BIRTH_PARAM}=${encodeURIComponent(expectedBirthWire(birth))}` : path;
}

// openRunCrossDaemon resolves the daemon URL serving the given run's
// store and navigates to its /runs/<id> route. `?store=<store_path>` is
// appended only when the destination daemon's own store is not the run's
// store (typically: a global ~/.iterion run viewed from a per-project
// daemon), so its read handlers route via the cross-store proxy
// (pkg/server/runs.go::resolveCrossStore) — a cross-store view, outside the
// expected-birth guarantee. A run of the destination's own store opens as
// that daemon's run and keeps it (crossDaemonRunStore), the row's birth
// carried in the URL (fullPageRunPath). Errors fall back to a same-daemon
// navigation so the worst case is the historical 404, not a swallowed click.
async function openRunCrossDaemon(run: GlobalActiveRun): Promise<void> {
  // A full document navigation, so wouter's router base does not apply and
  // the prefix is written out. STUDIO_BASE and not studioBase(): the
  // destination is another daemon's origin, which has no pane scope of ours.
  const target = `${STUDIO_BASE}/runs/${encodeURIComponent(run.id)}`;
  try {
    const daemonURL = await desktop.getDaemonURLForStore(run.store_path);
    if (daemonURL) {
      const current = await desktop.getServerURL().catch((err: unknown) => {
        console.warn("openRunCrossDaemon: GetServerURL failed — the run's store is named:", err);
        return "";
      });
      const store = crossDaemonRunStore(run, daemonURL, current);
      window.location.replace(daemonURL.replace(/\/$/, "") + fullPageRunPath(target, store, run.birth_id));
      return;
    }
  } catch (err) {
    if (typeof console !== "undefined") {
      console.warn("openRunCrossDaemon: GetDaemonURLForStore failed:", err);
    }
  }
  // Fallback: current daemon — so unlike the cross-daemon branch above, this
  // one is the SAME origin and must keep a workspace pane's /x/<id>. It names
  // the run's store unless it is this daemon's own.
  window.location.assign(scopePrefix() + fullPageRunPath(target, globalRunStoreParam(run), run.birth_id));
}

function GlobalRunRow({ run }: { run: GlobalActiveRun }) {
  const variant = STATUS_VARIANT[run.status] ?? "info";
  const label = labelForStatus(run.status);
  const location =
    run.workspace_dir ||
    // Strip the user's home so the path stays readable.
    run.store_path.replace(/^\/home\/[^/]+/, "~");

  // Pending state for the desktop cross-daemon click. The Wails
  // binding can block up to ~20s while spawning a fresh per-project
  // daemon (Go server boot + sandbox engine init). Without feedback
  // the row looks dead; flagging "Opening…" keeps the user oriented.
  const [opening, setOpening] = useState(false);

  // In desktop mode we resolve the correct daemon for this store (a
  // per-project daemon for project slots, the current daemon for the
  // global slot or any unrecognised path) and navigate via
  // window.location so the SPA re-bootstraps against the right
  // backend. In browser / non-Wails mode we fall back to a same-
  // daemon wouter link.
  const inner = (
    <>
      <LiveDot tone="info" size="md" className="shrink-0" />
      <div className="min-w-0 flex-1">
        <div className="text-xs font-semibold truncate">
          {run.name || run.workflow_name}
        </div>
        <div className="text-caption text-fg-subtle truncate">
          {run.workflow_name}
          {" · "}
          <span title={run.store_path}>{location}</span>
        </div>
      </div>
      <Badge variant={variant} className="shrink-0">
        {opening ? "Opening…" : label}
      </Badge>
      <span className="text-caption text-fg-subtle shrink-0 w-16 text-right">
        {formatRelative(run.updated_at)}
      </span>
    </>
  );

  if (isDesktop()) {
    return (
      <button
        type="button"
        disabled={opening}
        aria-busy={opening || undefined}
        onClick={() => {
          if (opening) return;
          setOpening(true);
          // openRunCrossDaemon navigates the window on success, so the
          // pending state only needs to clear when the call rejects
          // (the fallback navigation also unmounts the component).
          void openRunCrossDaemon(run).catch(() => setOpening(false));
        }}
        className="w-full px-4 py-2.5 flex items-center gap-3 text-left bg-info-soft/20 hover:bg-info-soft/40 border-l-2 border-info disabled:opacity-70 disabled:cursor-wait"
      >
        {inner}
      </button>
    );
  }
  // Non-desktop fallback (browser mode served by the daemon itself,
  // OR a transient render before Wails injects window.go.main.App).
  // Both observed: the SPA was loaded via http://<daemon>/ instead of
  // wails://wails.localhost/, so the click navigates same-daemon and
  // MUST still carry the store= query for a run of a foreign store so this
  // daemon's cross-store proxy can read from it. Without this,
  // /api/runs/<id> 404s with "open ... no such file or directory"
  // (observed: store_path=~/.iterion run viewed from a per-project
  // daemon). A run of this daemon's own store names none and carries the
  // row's birth in the navigation's state.
  const store = globalRunStoreParam(run);
  return (
    <Link
      href={withRunStore(`/runs/${encodeURIComponent(run.id)}`, store)}
      state={store ? undefined : navigationBirthState(run.id, run.birth_id)}
      className="w-full px-4 py-2.5 flex items-center gap-3 bg-info-soft/20 hover:bg-info-soft/40 border-l-2 border-info"
    >
      {inner}
    </Link>
  );
}
