import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";

import PageShell from "@/components/shared/PageShell";
import DispatcherControlBar from "@/components/shared/DispatcherControlBar";
import { Button } from "@/components/ui/Button";
import {
  cancelIssue,
  getState,
  getStatus,
  openWS,
  refresh as refreshTick,
  reload as reloadConfig,
  type DispatcherSnapshot,
  type ManagerStatus,
} from "@/api/dispatcher";
import SettingsDrawer from "@/components/Dispatcher/SettingsDrawer";
import TrackerErrorBanner from "@/components/shared/TrackerErrorBanner";

export default function DispatcherView() {
  const [, setLocation] = useLocation();
  const [snap, setSnap] = useState<DispatcherSnapshot | null>(null);
  const [status, setStatus] = useState<ManagerStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const openRun = useCallback(
    (runID: string) => setLocation(`/runs/${encodeURIComponent(runID)}`),
    [setLocation],
  );

  const reload = useCallback(async () => {
    setError(null);
    try {
      const s = await getState();
      setSnap(s);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
    let cancelled = false;
    let retryTimer: number | null = null;
    let attempt = 0;

    const scheduleRetry = () => {
      if (cancelled) return;
      // Exponential backoff capped at 30s. The prior implementation
      // hammered the server at 1.5s intervals indefinitely whenever
      // the dispatcher was offline.
      const delay = Math.min(1500 * 2 ** attempt, 30_000);
      attempt += 1;
      retryTimer = window.setTimeout(() => void connect(), delay);
    };

    const connect = async () => {
      if (cancelled) return;
      try {
        const ws = await openWS();
        if (cancelled) {
          ws.close();
          return;
        }
        attempt = 0;
        wsRef.current = ws;
        ws.onmessage = (e) => {
          try {
            setSnap(JSON.parse(e.data) as DispatcherSnapshot);
          } catch {
            // ignore malformed frames
          }
        };
        ws.onclose = () => {
          if (cancelled) return;
          // Drop our ref so a stale handler can't observe the closed
          // socket as "live" on the next render.
          if (wsRef.current === ws) wsRef.current = null;
          scheduleRetry();
        };
        ws.onerror = () => ws.close();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        scheduleRetry();
      }
    };
    void connect();
    return () => {
      cancelled = true;
      if (retryTimer != null) {
        window.clearTimeout(retryTimer);
        retryTimer = null;
      }
      const ws = wsRef.current;
      wsRef.current = null;
      if (ws) {
        // Detach close handler so the in-flight FIN doesn't kick off
        // a new reconnect cycle after the effect tore down.
        ws.onclose = null;
        ws.onmessage = null;
        ws.onerror = null;
        ws.close();
      }
    };
  }, [reload]);

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const s = await getStatus();
        if (!cancelled) setStatus(s);
      } catch {
        if (!cancelled) setStatus(null);
      }
    };
    void tick();
    const t = window.setInterval(() => void tick(), 2000);
    return () => {
      cancelled = true;
      window.clearInterval(t);
    };
  }, []);

  const canOperate = status?.state === "running" || status?.state === "paused";

  const doRefresh = useCallback(async () => {
    try {
      await refreshTick();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);
  const doReload = useCallback(async () => {
    try {
      await reloadConfig();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);
  const doCancel = useCallback(async (issueID: string) => {
    if (!confirm(`Cancel run for ${issueID}?`)) return;
    try {
      await cancelIssue(issueID);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  if (loading) {
    return (
      <PageShell active="dispatcher">
        <div className="p-8 text-fg-muted">Loading dispatcher state…</div>
      </PageShell>
    );
  }
  if (!snap) {
    return (
      <PageShell active="dispatcher">
        <div className="p-8 text-fg-muted">
          Dispatcher not available.{" "}
          <code className="text-xs">iterion dispatch &lt;config.yaml&gt;</code> exposes this view.
        </div>
      </PageShell>
    );
  }

  const running = snap.running ?? [];
  const retries = snap.retries ?? [];

  return (
    <PageShell
      active="dispatcher"
      rightActions={
        <>
          <Button
            variant="secondary"
            size="sm"
            disabled={!canOperate}
            title={canOperate ? undefined : "Start the dispatcher first"}
            onClick={() => void doRefresh()}
          >
            Force tick
          </Button>
          <Button
            variant="secondary"
            size="sm"
            disabled={!canOperate}
            title={canOperate ? undefined : "Start the dispatcher first"}
            onClick={() => void doReload()}
          >
            Reload config
          </Button>
        </>
      }
    >
      <DispatcherControlBar onOpenSettings={() => setSettingsOpen(true)} />
      <SettingsDrawer
        open={settingsOpen}
        onClose={() => setSettingsOpen(false)}
        onSaved={() => void reload()}
      />

      {error && (
        <div className="bg-red-500/10 border-b border-red-500/40 px-4 py-2 text-xs text-red-200">
          {error}
        </div>
      )}

      {snap.last_tracker_error && (
        <TrackerErrorBanner
          tracker={snap.tracker}
          message={snap.last_tracker_error}
        />
      )}

      <main className="flex-1 overflow-auto p-4 space-y-4 max-w-4xl">
        <SummaryCard snap={snap} />
        <RunningTable
          rows={running}
          stallTimeoutS={snap.stall_timeout_seconds}
          onCancel={doCancel}
          onOpenRun={openRun}
        />
        <RetriesTable
          rows={retries}
          onFocusIssue={(id) =>
            setLocation(`/board?focus=${encodeURIComponent(id)}`)
          }
          onRefreshNow={() => void doRefresh()}
        />
      </main>
    </PageShell>
  );
}

function SummaryCard({ snap }: { snap: DispatcherSnapshot }) {
  return (
    <section className="rounded border border-border-default bg-surface-1 p-4">
      <h2 className="text-sm font-semibold mb-2">{snap.name || "Dispatcher"}</h2>
      <dl className="grid grid-cols-2 sm:grid-cols-4 gap-x-4 gap-y-1 text-xs">
        <KV k="Tracker" v={snap.tracker} />
        <KV k="Polling" v={`${snap.polling_interval_seconds}s`} />
        <KV k="Stall timeout" v={`${snap.stall_timeout_seconds}s`} />
        <KV k="Slots" v={`${snap.slots.global_used} / ${snap.slots.global_max}`} />
      </dl>
      {snap.slots.per_state_max && Object.keys(snap.slots.per_state_max).length > 0 && (
        <div className="mt-2 text-xs text-fg-muted">
          per-state caps:{" "}
          {Object.entries(snap.slots.per_state_max).map(([s, max]) => (
            <span key={s} className="ml-2">
              {s}: {snap.slots.per_state_used?.[s] ?? 0}/{max}
            </span>
          ))}
        </div>
      )}
    </section>
  );
}

// stallStyle inspects how long a running entry has been silent relative
// to the dispatcher's stallTimeout and returns a (className, hint) pair
// for the row. The thresholds match what the operator can act on:
//   ≥ 50% of the budget elapsed → amber  ("slow — keep an eye")
//   ≥ 100% of the budget        → red    ("about to be cancelled")
// Below 50%: no decoration, the row reads as normal.
function stallStyle(
  lastEventAt: string,
  stallTimeoutS: number,
): { rowClass: string; hint: string | null } {
  if (!stallTimeoutS || stallTimeoutS <= 0) {
    return { rowClass: "", hint: null };
  }
  const last = Date.parse(lastEventAt);
  if (!Number.isFinite(last)) return { rowClass: "", hint: null };
  const elapsedS = (Date.now() - last) / 1000;
  if (elapsedS >= stallTimeoutS) {
    return {
      rowClass: "bg-red-500/10",
      hint: `Silent for ${Math.round(elapsedS)}s ≥ stall timeout (${Math.round(stallTimeoutS)}s) — will be cancelled on the next reconciliation tick.`,
    };
  }
  if (elapsedS >= stallTimeoutS / 2) {
    return {
      rowClass: "bg-amber-500/10",
      hint: `Silent for ${Math.round(elapsedS)}s — half the stall budget (${Math.round(stallTimeoutS)}s) consumed.`,
    };
  }
  return { rowClass: "", hint: null };
}

function RunningTable({
  rows,
  stallTimeoutS,
  onCancel,
  onOpenRun,
}: {
  rows: DispatcherSnapshot["running"];
  stallTimeoutS: number;
  onCancel: (id: string) => void;
  onOpenRun: (runID: string) => void;
}) {
  return (
    <section className="rounded border border-border-default bg-surface-1">
      <header className="px-4 py-2 border-b border-border-default text-sm font-semibold">
        Running ({rows?.length ?? 0})
      </header>
      {!rows || rows.length === 0 ? (
        <div className="p-4 text-xs text-fg-muted">No runs in flight.</div>
      ) : (
        <table className="w-full text-xs">
          <thead className="text-fg-muted border-b border-border-default">
            <tr>
              <th className="text-left py-1.5 px-3 font-normal">Identifier</th>
              <th className="text-left py-1.5 px-3 font-normal">Run</th>
              <th className="text-left py-1.5 px-3 font-normal">State</th>
              <th className="text-left py-1.5 px-3 font-normal">Started</th>
              <th className="text-left py-1.5 px-3 font-normal">Last event</th>
              <th className="text-right py-1.5 px-3 font-normal">Actions</th>
            </tr>
          </thead>
          <tbody>
            {rows!.map((r) => {
              const stall = stallStyle(r.last_event_at, stallTimeoutS);
              return (
              <tr
                key={r.issue_id}
                className={`border-b border-border-default/60 ${stall.rowClass}`}
                title={stall.hint ?? undefined}
              >
                <td className="py-1.5 px-3 font-mono">{r.identifier}</td>
                <td className="py-1.5 px-3 font-mono truncate max-w-[14rem]">
                  <button
                    type="button"
                    onClick={() => onOpenRun(r.run_id)}
                    className="text-info hover:underline"
                    title={`Open run ${r.run_id}`}
                  >
                    {r.run_id}
                  </button>
                </td>
                <td className="py-1.5 px-3">{r.workflow_state}</td>
                <td className="py-1.5 px-3 text-fg-muted">{relTime(r.started_at)}</td>
                <td className="py-1.5 px-3 text-fg-muted">
                  {r.last_event_name ? r.last_event_name + " · " : ""}
                  {relTime(r.last_event_at)}
                  {stall.hint && (
                    <span className="ml-1 text-amber-300/90">⏱</span>
                  )}
                </td>
                <td className="py-1.5 px-3 text-right">
                  <button
                    onClick={() => onCancel(r.issue_id)}
                    className="text-[11px] px-2 py-0.5 rounded border border-border-default hover:bg-surface-2"
                  >
                    Cancel
                  </button>
                </td>
              </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}

// useTick re-renders the caller at intervalMs while `active`. Used by
// RetriesTable to keep countdowns smooth without a full dispatcher poll
// each second — the retry table only needs to recompute due_at minus
// now() on its own clock.
function useTick(intervalMs: number, active: boolean): number {
  const [tick, setTick] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setTick(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs, active]);
  return tick;
}

// formatRetryDue returns a short human label for "in 12s" / "due now"
// derived purely from due_at + now. Lives next to RetriesTable so the
// formatting stays scoped to the retry context (the rest of the page
// uses relTime).
function formatRetryDue(dueIso: string, nowMs: number): string {
  if (!dueIso) return "";
  const due = Date.parse(dueIso);
  if (!Number.isFinite(due)) return "";
  const deltaS = Math.round((due - nowMs) / 1000);
  if (deltaS <= 0) return "due";
  if (deltaS < 60) return `in ${deltaS}s`;
  if (deltaS < 3600) return `in ${Math.round(deltaS / 60)}m`;
  return `in ${Math.round(deltaS / 3600)}h`;
}

function RetriesTable({
  rows,
  onFocusIssue,
  onRefreshNow,
}: {
  rows: DispatcherSnapshot["retries"];
  onFocusIssue: (issueID: string) => void;
  onRefreshNow: () => void;
}) {
  // Tick every 1s when at least one retry is due in under 5 minutes so
  // the countdown is responsive without burning CPU on long-deferred
  // queues.
  const needsTick = (rows ?? []).some((r) => {
    const due = Date.parse(r.due_at);
    return Number.isFinite(due) && due - Date.now() < 5 * 60_000;
  });
  const now = useTick(1000, needsTick);
  return (
    <section className="rounded border border-border-default bg-surface-1">
      <header className="px-4 py-2 border-b border-border-default text-sm font-semibold flex items-center justify-between gap-2">
        <span>Retry queue ({rows?.length ?? 0})</span>
        {rows && rows.length > 0 && (
          <button
            type="button"
            onClick={onRefreshNow}
            className="text-[11px] px-2 py-0.5 rounded border border-border-default hover:bg-surface-2 text-fg-muted hover:text-fg-default"
            title="Trigger an immediate tracker poll; due retries will fire on the next tick."
          >
            Poll now
          </button>
        )}
      </header>
      {!rows || rows.length === 0 ? (
        <div className="p-4 text-xs text-fg-muted">No retries pending.</div>
      ) : (
        <table className="w-full text-xs">
          <thead className="text-fg-muted border-b border-border-default">
            <tr>
              <th className="text-left py-1.5 px-3 font-normal">Issue</th>
              <th className="text-left py-1.5 px-3 font-normal">Attempt</th>
              <th className="text-left py-1.5 px-3 font-normal">Due</th>
              <th className="text-left py-1.5 px-3 font-normal">Last error</th>
            </tr>
          </thead>
          <tbody>
            {rows!.map((r) => {
              const dueLabel = formatRetryDue(r.due_at, now);
              const isDue = dueLabel === "due";
              return (
              <tr
                key={r.issue_id}
                className={`border-b border-border-default/60 hover:bg-surface-2/40 cursor-pointer ${
                  isDue ? "bg-amber-500/5" : ""
                }`}
                onClick={() => onFocusIssue(r.issue_id)}
                title="Open this issue on the board"
              >
                <td className="py-1.5 px-3 font-mono">{r.identifier || r.issue_id}</td>
                <td className="py-1.5 px-3">{r.attempt}</td>
                <td className="py-1.5 px-3">
                  <span className={isDue ? "text-amber-300" : "text-fg-muted"}>
                    {dueLabel || relTime(r.due_at)}
                  </span>
                </td>
                <td className="py-1.5 px-3 text-red-300/80 truncate max-w-[24rem]">
                  {r.error}
                </td>
              </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <>
      <dt className="text-fg-muted">{k}</dt>
      <dd>{v}</dd>
    </>
  );
}

function relTime(iso: string): string {
  if (!iso) return "";
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return iso;
  const dt = (Date.now() - t) / 1000;
  if (dt < 0) {
    const abs = Math.abs(dt);
    if (abs < 60) return `in ${Math.round(abs)}s`;
    if (abs < 3600) return `in ${Math.round(abs / 60)}m`;
    return `in ${Math.round(abs / 3600)}h`;
  }
  if (dt < 60) return `${Math.round(dt)}s ago`;
  if (dt < 3600) return `${Math.round(dt / 60)}m ago`;
  return `${Math.round(dt / 3600)}h ago`;
}
