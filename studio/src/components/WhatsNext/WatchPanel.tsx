import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";
import { Link } from "wouter";

import { queueMessage } from "@/api/queueMessages";
import { transitionIssue } from "@/api/native";
import { addWatch } from "@/api/runs";
import { stateChipStyle } from "@/lib/board/stateTheme";
import { useServerInfoStore } from "@/store/serverInfo";
import { shortIssueId } from "@/lib/whats-next/issueId";
import {
  formatUpdatesAsChatMessage,
  useWatchList,
  type WatchEntry,
} from "@/lib/whats-next/useWatchList";

interface WatchPanelProps {
  runId: string | null;
}

export default function WatchPanel({ runId }: WatchPanelProps) {
  const { entries, pendingUpdates, acknowledgeUpdates } = useWatchList(runId);

  const [forwardError, setForwardError] = useState<string | null>(null);
  const [forwarding, setForwarding] = useState(false);

  if (entries.length === 0) return null;

  const onTellNexie = async () => {
    if (!runId || pendingUpdates.length === 0) return;
    setForwardError(null);
    setForwarding(true);
    try {
      await queueMessage(runId, formatUpdatesAsChatMessage(pendingUpdates));
      acknowledgeUpdates();
    } catch (e) {
      setForwardError(errorMessage(e) ?? String(e));
    } finally {
      setForwarding(false);
    }
  };

  return (
    <div
      className="border-b border-border-subtle bg-surface-1/60 px-4 py-2"
      role="region"
      aria-label="Dispatched items"
    >
      <div className="flex items-center justify-between gap-2 mb-1">
        <div className="text-caption uppercase tracking-wide text-fg-subtle">
          Watching · {entries.length} dispatched
          {pendingUpdates.length > 0 && (
            <span className="ml-2 text-warning-fg normal-case tracking-normal">
              · {pendingUpdates.length} update{pendingUpdates.length === 1 ? "" : "s"}
            </span>
          )}
        </div>
        <div className="flex items-center gap-2">
          {pendingUpdates.length > 0 && runId && (
            <button
              type="button"
              onClick={() => void onTellNexie()}
              disabled={forwarding}
              className="text-micro text-accent-text hover:underline cursor-pointer disabled:opacity-50 disabled:cursor-wait"
              title="Forward these state changes into Nexie's chat so she can react on her next turn."
            >
              {forwarding ? "Forwarding…" : "Tell Nexie"}
            </button>
          )}
          <Link href="/board" className="text-caption text-accent-text hover:underline">
            board ↗
          </Link>
        </div>
      </div>
      {forwardError && (
        <div className="mb-1 text-caption text-danger-fg" title={forwardError}>
          Forward failed: {truncate(forwardError, 80)}
        </div>
      )}
      <ul className="flex flex-col gap-1">
        {entries.map((entry) => (
          <WatchRow key={entry.issueId} entry={entry} runId={runId} />
        ))}
      </ul>
    </div>
  );
}

function WatchRow({ entry, runId }: { entry: WatchEntry; runId: string | null }) {
  const { issue, issueId, lastFetchError } = entry;
  const title = issue?.title ?? shortIssueId(issueId);
  // The dispatch affordance mutates the native board (transitionIssue),
  // so it only exists on servers with the native tracker enabled.
  const nativeTrackerEnabled = useServerInfoStore(
    (s) => !!s.info?.native_tracker_enabled,
  );
  // Optimistic local override: once the operator clicks Dispatch we
  // flip the chip to "ready" immediately rather than wait for the
  // (up to 15s) poll. The poll's real state takes over on its next
  // tick; this only bridges the gap so the click feels instant.
  const [optimisticReady, setOptimisticReady] = useState(false);
  const [dispatching, setDispatching] = useState(false);
  const [dispatchError, setDispatchError] = useState<string | null>(null);

  const state = optimisticReady ? "ready" : issue?.state ?? "…";
  const lastRunId = issue?.last_run_id ?? null;
  // Dispatchable = currently in backlog. Promoting backlog → ready is
  // what the dispatcher's polling actor waits for; from any other
  // state the button is meaningless (already moving / done).
  const canDispatch = nativeTrackerEnabled && issue?.state === "backlog" && !optimisticReady;

  const onDispatch = async () => {
    setDispatching(true);
    setDispatchError(null);
    try {
      await transitionIssue(issueId, "ready");
      setOptimisticReady(true);
      // Persist the subscription server-side (best-effort) so the watch
      // coordinator fans the dispatched bot's *future* transitions back
      // to this run, and so the watch survives a reload. Fired after the
      // backlog → ready transition so the operator's own dispatch move
      // doesn't self-notify. Failure is non-fatal: the event-derived
      // watch still works for the live session.
      if (runId) void addWatch(runId, issueId).catch(() => {});
    } catch (e) {
      setDispatchError(errorMessage(e) ?? String(e));
    } finally {
      setDispatching(false);
    }
  };

  return (
    <li className="flex items-center gap-2 text-body text-fg-default">
      <span
        className="shrink-0 inline-flex items-center px-1.5 py-0.5 rounded text-caption font-medium uppercase tracking-wide"
        style={stateChipStyle(state)}
      >
        {state}
      </span>
      <span className="flex-1 min-w-0 truncate" title={title}>
        {title}
      </span>
      {dispatchError && (
        <span className="text-caption text-danger-fg" title={dispatchError}>
          (failed)
        </span>
      )}
      {lastFetchError && !dispatchError && (
        <span className="text-caption text-fg-subtle" title={lastFetchError}>
          (stale)
        </span>
      )}
      {canDispatch && (
        <button
          type="button"
          onClick={() => void onDispatch()}
          disabled={dispatching}
          className="text-micro text-accent-text hover:underline shrink-0 cursor-pointer disabled:opacity-50 disabled:cursor-wait"
          title="Push this ticket from backlog to ready — the dispatcher picks it up."
        >
          {dispatching ? "…" : "▶ dispatch"}
        </button>
      )}
      {lastRunId && (
        <Link
          href={`/runs/${encodeURIComponent(lastRunId)}`}
          className="text-micro text-accent-text hover:underline shrink-0"
          title={`Open last run ${lastRunId}`}
        >
          run ↗
        </Link>
      )}
    </li>
  );
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}
