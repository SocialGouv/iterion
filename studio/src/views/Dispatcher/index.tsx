import { clickableRowProps } from "@/lib/a11y";
import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import DispatcherControlBar from "@/components/shared/DispatcherControlBar";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { EmptyState } from "@/components/ui/EmptyState";
import { Tooltip } from "@/components/ui";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import {
  cancelIssue,
  getState,
  getStatus,
  openWS,
  refresh as refreshTick,
  reload as reloadConfig,
  type DispatchSkipView,
  type DispatcherSnapshot,
  type ManagerStatus,
} from "@/api/dispatcher";
import SettingsDrawer from "@/components/Dispatcher/SettingsDrawer";
import TrackerErrorBanner from "@/components/shared/TrackerErrorBanner";
import CostCapBanner from "@/components/shared/CostCapBanner";
import { dispatcherActionState } from "./dispatcherActionState";
import { dispatcherPillMeta } from "@/components/shared/dispatcherPillMeta";

import RetriesTable from "./RetriesTable";
import RunningTable from "./RunningTable";

export default function DispatcherView() {
  const [, setLocation] = useLocation();
  const [snap, setSnap] = useState<DispatcherSnapshot | null>(null);
  const [status, setStatus] = useState<ManagerStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [settingsOpen, setSettingsOpen] = useState(false);
  // Consecutive failed status polls — surfaces an explicit "manager
  // unreachable" banner once the silent 2s retry has failed a few times,
  // instead of only quietly flipping the chip to "unreachable".
  const [pollFails, setPollFails] = useState(0);
  // One useAsyncAction underpins every action button (reload, refresh,
  // reload-config, cancel). Sharing a single error slot keeps the page
  // to one InlineBanner; the hand-rolled try/catch ladder previously
  // had the same effective behaviour but had to repeat
  // setError(null)/setError(errorMessage(e)) at each call site.
  const action = useAsyncAction();
  const { confirm, dialog } = useConfirm();
  const wsRef = useRef<WebSocket | null>(null);
  // The dispatcher manager's WS only exists while it is running or paused;
  // idle / error / absent all 503. Gate the live socket on that so an
  // unconfigured dashboard doesn't spin an endless failed-handshake loop
  // that spams the console (~1 error every 1.5s).
  const dispatcherAttached =
    status?.state === "running" || status?.state === "paused";
  const openRun = useCallback(
    (runID: string) => setLocation(`/runs/${encodeURIComponent(runID)}`),
    [setLocation],
  );

  // Destructure the stable setters/runners so dependency arrays don't
  // re-fire effects every render — useAsyncAction wraps `run` and
  // `setError` in useCallback for exactly this reason.
  const { run: actionRun, setError: setActionError } = action;
  const reload = useCallback(async () => {
    await actionRun(async () => {
      const s = await getState();
      setSnap(s);
    });
    setLoading(false);
  }, [actionRun]);

  useEffect(() => {
    void reload();
    // Only the running/paused manager serves the WS; skip it while idle so
    // we don't loop on 503s. The 2s status poll re-runs this effect once
    // the dispatcher attaches.
    if (!dispatcherAttached) return;
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
        wsRef.current = ws;
        // Reset the backoff only on a real connection: openWS resolves a
        // still-connecting socket, so a 503 handshake failure surfaces
        // later via onerror/onclose. Resetting here would defeat the
        // backoff and retry every 1.5s.
        ws.onopen = () => {
          attempt = 0;
        };
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
        // Surface the connection error in the page banner via the
        // shared action slot (the WS path doesn't go through actionRun)
        // and schedule a backoff retry.
        setActionError(errorMessage(e));
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
  }, [reload, dispatcherAttached, setActionError]);

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const s = await getStatus();
        if (!cancelled) {
          setStatus(s);
          setPollFails(0);
        }
      } catch {
        if (!cancelled) {
          setStatus(null);
          setPollFails((n) => n + 1);
        }
      }
    };
    void tick();
    const t = window.setInterval(() => void tick(), 2000);
    return () => {
      cancelled = true;
      window.clearInterval(t);
    };
  }, []);

  const actions = dispatcherActionState(status?.state, snap?.paused ?? false);

  const doRefresh = useCallback(
    () => void actionRun(refreshTick),
    [actionRun],
  );
  const doReload = useCallback(
    () => void actionRun(reloadConfig),
    [actionRun],
  );
  const doCancel = useCallback(
    async (issueID: string) => {
      const ok = await confirm({
        title: "Cancel in-flight run?",
        message: `Cancel the in-flight run for ${issueID}? The issue stays on the board and can be re-dispatched.`,
        confirmLabel: "Cancel run",
        confirmVariant: "danger",
      });
      if (!ok) return;
      await actionRun(() => cancelIssue(issueID));
    },
    [confirm, actionRun],
  );

  useHeaderSlot({
    left: <span className="text-xs font-medium text-fg-default">Dispatcher</span>,
    right: (
      <>
        <Tooltip content={actions.pollTitle}>
          <Button
            variant="secondary"
            size="sm"
            disabled={!actions.canPollDispatches}
            onClick={doRefresh}
          >
            Poll now
          </Button>
        </Tooltip>
        <Tooltip content={actions.reloadTitle}>
          <Button
            variant="secondary"
            size="sm"
            disabled={!actions.canReloadConfig}
            onClick={doReload}
          >
            Reload config
          </Button>
        </Tooltip>
      </>
    ),
  });

  if (loading) {
    return <EmptyState message="Loading dispatcher state…" />;
  }
  if (!snap) {
    return (
      <div className="p-8 text-fg-muted">
        Dispatcher not available.{" "}
        <code className="text-xs">iterion dispatch &lt;config.yaml&gt;</code> exposes this view.
      </div>
    );
  }

  const running = snap.running ?? [];
  const retries = snap.retries ?? [];
  // Eligible issues the dispatcher refused to claim this scan because
  // their explicit `bot` is unresolvable / unrouteable. Surfaced here so
  // a misconfigured `bot:` is visible on the dashboard too (the board
  // already badges the affected cards) — otherwise an operator watching
  // this view sees an eligible issue that simply never runs, with no
  // reason given.
  const skips = snap.dispatch_skips ?? [];

  return (
    <div className="h-full flex flex-col overflow-hidden">
      {dialog}
      <DispatcherControlBar onOpenSettings={() => setSettingsOpen(true)} />
      <SettingsDrawer
        open={settingsOpen}
        onClose={() => setSettingsOpen(false)}
        onSaved={() => void reload()}
      />

      {action.error && <InlineBanner tone="danger">{action.error}</InlineBanner>}

      {pollFails >= 3 && (
        <InlineBanner tone="warning">
          Dispatcher manager unreachable — retrying every 2s ({pollFails} failed
          checks).
        </InlineBanner>
      )}

      {snap.last_tracker_error && (
        <TrackerErrorBanner
          tracker={snap.tracker}
          message={snap.last_tracker_error}
        />
      )}

      {/* Daily spend-cap banner — self-polls /api/v1/limits/cost and
          renders only when the cap is reached. The dispatcher gate has
          already stopped new dispatches; this surfaces it + the
          override action. */}
      <CostCapBanner />

      {snap.paused && (
        <InlineBanner tone="warning" title="Dispatcher paused">
          New dispatches are suspended. The retry queue won't fire, and new ready
          issues won't be picked up. In-flight runs continue. Resume from the toolbar above.
        </InlineBanner>
      )}

      <div className="flex-1 overflow-auto p-4 space-y-4 max-w-4xl">
        <SummaryCard snap={snap} status={status} />
        <DispatchSkipsTable
          rows={skips}
          onFocusIssue={(id) =>
            setLocation(`/board?focus=${encodeURIComponent(id)}`)
          }
        />
        <RunningTable
          rows={running}
          stallTimeoutS={snap.stall_timeout_seconds}
          onCancel={doCancel}
          onOpenRun={openRun}
        />
        <RetriesTable
          rows={retries}
          canPollDispatches={actions.canPollDispatches}
          pollTitle={actions.pollTitle}
          onFocusIssue={(id) =>
            setLocation(`/board?focus=${encodeURIComponent(id)}`)
          }
          onRefreshNow={doRefresh}
        />
      </div>
    </div>
  );
}

function SummaryCard({
  snap,
  status,
}: {
  snap: DispatcherSnapshot;
  status: ManagerStatus | null;
}) {
  // Chip reflects manager LIFECYCLE state (idle/running/paused/error),
  // not just the actor's runtime `paused` flag. Previously the chip
  // read `snap.paused` only, so it showed "active" whenever the actor
  // existed but wasn't operator-paused — including transient windows
  // where the manager was idle/error/initialising. Reuse the same pill
  // metadata as the DispatcherControlBar so both chips stay in sync.
  const pillState = status ? status.state : "unreachable";
  const meta = dispatcherPillMeta(pillState);
  return (
    <section className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)] p-4">
      <div className="flex items-center justify-between mb-2 gap-3">
        <h2 className="text-title font-semibold">{snap.name || "Dispatcher"}</h2>
        <Tooltip content={meta.title}>
          <span
            className={`text-caption font-mono rounded px-1.5 py-0.5 ${meta.className}`}
          >
            {pillState}
          </span>
        </Tooltip>
      </div>
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

// DispatchSkipsTable surfaces eligible issues the dispatcher refused to
// claim because their explicit `bot` can't be resolved to a workflow file
// or has no dispatch route (not in assignee_workflows). Mirrors the board's
// per-card "won't dispatch" badge so the failure is visible on the
// dashboard too — without it, the dispatch_skips snapshot field (already
// wired through state.go + the WS payload) reached only the board, and an
// operator watching this view saw an eligible issue silently never run.
//
// Renders nothing when there are no skips (the common case) — this is an
// exceptional misconfiguration surface, not a steady-state queue like
// running/retries, so it stays out of the way until it has something to
// say. Rows are click-to-focus the offending issue on the board, where the
// editor lets the operator fix the bot.
function DispatchSkipsTable({
  rows,
  onFocusIssue,
}: {
  rows: DispatchSkipView[];
  onFocusIssue: (issueID: string) => void;
}) {
  if (rows.length === 0) return null;
  return (
    <section className="rounded border border-danger/40 bg-danger-soft">
      <header className="px-4 py-2 border-b border-danger/30 text-sm font-semibold text-danger-fg flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
        <span>⚠ Won&apos;t dispatch ({rows.length})</span>
        <span className="text-micro font-normal text-danger-fg/70">
          eligible issues the dispatcher refuses to claim — fix the{" "}
          <code>bot</code> in the issue editor or add it to{" "}
          <code>assignee_workflows</code>
        </span>
      </header>
      <div className="overflow-x-auto">
        <table className="min-w-full text-xs">
          <thead className="text-fg-muted border-b border-border-default">
            <tr>
              <th className="text-left py-1.5 px-3 font-normal whitespace-nowrap">Issue</th>
              <th className="text-left py-1.5 px-3 font-normal">Bot</th>
              <th className="text-left py-1.5 px-3 font-normal">Reason</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((s) => (
              <tr
                key={s.issue_id}
                className="border-b border-border-default/60 hover:bg-surface-2/40 cursor-pointer focus-visible:bg-surface-2/60 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent"
                {...clickableRowProps(() => onFocusIssue(s.issue_id), `Open issue ${s.identifier || s.issue_id} on the board to fix its bot`)}
              >
                <td className="py-1.5 px-3 font-mono whitespace-nowrap">
                  {s.identifier || s.issue_id}
                </td>
                <td className="py-1.5 px-3 font-mono">
                  {s.bot ? s.bot : <span className="text-fg-subtle">—</span>}
                </td>
                <td className="py-1.5 px-3 text-danger-fg/80">{s.reason}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
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
