import { useCallback, useEffect, useState } from "react";
import { Link } from "wouter";
import { ExternalLinkIcon } from "@radix-ui/react-icons";

import type { IssuesSummaryMessage } from "@/lib/whats-next/messages";
import { Badge } from "@/components/ui";
import { useServerInfoStore } from "@/store/serverInfo";
import * as dispatcher from "@/api/dispatcher";

interface Props {
  message: IssuesSummaryMessage;
}

export default function IssuesSummaryCard({ message }: Props) {
  const serverInfo = useServerInfoStore((s) => s.info);
  const boardEnabled = serverInfo?.native_tracker_enabled ?? false;
  const dispatcherEnabled = serverInfo?.dispatcher_enabled ?? false;
  const { createdIssues, failedIssues, planPath, summary } = message;
  const dispatcherStatus = useDispatcherStatusPoll({
    enabled: dispatcherEnabled,
  });

  return (
    <div className="rounded-lg border border-success/40 bg-success-soft p-3 space-y-3">
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="text-[13px] font-semibold text-success-fg">
          Plan materialised
        </h3>
        <span className="text-[10px] text-fg-subtle font-mono">{message.nodeId}</span>
      </div>

      <p className="text-[12px] text-fg-default whitespace-pre-wrap break-words">{summary}</p>

      {createdIssues.length > 0 && (
        <div className="space-y-1.5">
          <div className="text-[10px] uppercase tracking-wide font-medium text-fg-muted">
            Issues created ({createdIssues.length})
          </div>
          <ul className="space-y-1">
            {createdIssues.map((it) => (
              <li
                key={it.id}
                className="text-[12px] flex items-baseline gap-2 rounded bg-surface-1 border border-border-subtle px-2 py-1"
              >
                <code className="text-[10px] text-fg-subtle shrink-0">
                  {it.id.slice(0, 12)}
                </code>
                <span className="flex-1 truncate text-fg-default">{it.title}</span>
                <HorizonBadge horizon={it.horizon} />
                {it.assignee && (
                  <Badge variant="neutral" size="sm">
                    {it.assignee}
                  </Badge>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}

      {failedIssues.length > 0 && (
        <div className="space-y-1.5">
          <div className="text-[10px] uppercase tracking-wide font-medium text-danger-fg">
            Failed ({failedIssues.length})
          </div>
          <ul className="space-y-1">
            {failedIssues.map((it, i) => (
              <li
                key={i}
                className="text-[11px] rounded bg-danger-soft border border-danger/40 px-2 py-1 text-danger-fg"
              >
                <span className="font-medium">{it.title}</span>
                <span className="text-fg-muted"> — {it.error}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {planPath && (
        <div className="text-[11px] text-fg-muted">
          Audit markdown:{" "}
          <code className="text-[11px] font-mono text-fg-default break-all">
            {planPath}
          </code>
        </div>
      )}

      <div className="flex items-center gap-2 pt-2 border-t border-border-subtle">
        {boardEnabled && (
          <Link
            href="/board"
            className="inline-flex items-center gap-1 text-[12px] text-accent hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            Open board
          </Link>
        )}
        {dispatcherEnabled && (
          <Link
            href="/dispatcher"
            className="inline-flex items-center gap-1 text-[12px] text-accent hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            Open dispatcher
          </Link>
        )}
        {dispatcherEnabled && <DispatcherChip status={dispatcherStatus} />}
      </div>
    </div>
  );
}

// useDispatcherStatusPoll polls /api/v1/dispatcher/status while the
// IssuesSummaryCard is mounted. Returns null until the first response
// lands (or never, if dispatcher is disabled / unreachable). Cheap —
// the status payload is tiny and a 4s tick is well under any reasonable
// dispatch interval.
function useDispatcherStatusPoll({
  enabled,
  intervalMs = 4000,
}: {
  enabled: boolean;
  intervalMs?: number;
}) {
  const [status, setStatus] = useState<dispatcher.ManagerStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    if (!enabled) return;
    try {
      const s = await dispatcher.getStatus();
      setStatus(s);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [enabled]);

  useEffect(() => {
    if (!enabled) return;
    void refresh();
    const t = setInterval(() => void refresh(), intervalMs);
    return () => clearInterval(t);
  }, [enabled, refresh, intervalMs]);

  const startDispatcher = useCallback(async () => {
    setBusy(true);
    try {
      const s = await dispatcher.start();
      setStatus(s);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }, []);

  return { status, busy, error, startDispatcher };
}

function DispatcherChip({
  status,
}: {
  status: ReturnType<typeof useDispatcherStatusPoll>;
}) {
  const state = status.status?.state ?? "idle";
  const hasConfig = status.status?.has_config ?? false;
  const label =
    state === "running"
      ? "Dispatcher: running"
      : state === "paused"
        ? "Dispatcher: paused"
        : state === "error"
          ? "Dispatcher: error"
          : hasConfig
            ? "Dispatcher: idle"
            : "Dispatcher: not configured";
  const cls =
    state === "running"
      ? "bg-success-soft text-success-fg border-success/40"
      : state === "error"
        ? "bg-danger-soft text-danger-fg border-danger/40"
        : "bg-surface-2 text-fg-muted border-border-subtle";
  return (
    <div className="ml-auto flex items-center gap-2">
      {status.error && (
        <span
          className="text-[10px] text-danger-fg truncate max-w-[180px]"
          title={status.error}
        >
          ⚠ {status.error}
        </span>
      )}
      <span
        className={`inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] ${cls}`}
        title={
          status.status?.started_at
            ? `since ${status.status.started_at}`
            : undefined
        }
      >
        {label}
      </span>
      {state === "idle" && hasConfig && (
        <button
          type="button"
          disabled={status.busy}
          onClick={() => void status.startDispatcher()}
          className="rounded border border-border-default px-2 py-0.5 text-[11px] hover:bg-surface-2 disabled:opacity-50"
          title="Start the dispatcher — it will pick up tickets in the `ready` state"
        >
          {status.busy ? "…" : "▶ Start"}
        </button>
      )}
      {!hasConfig && (
        <Link
          href="/dispatcher"
          className="rounded border border-border-default px-2 py-0.5 text-[11px] text-accent hover:bg-surface-2"
          title="Configure the dispatcher (workflow + tracker)"
        >
          Configure
        </Link>
      )}
    </div>
  );
}

function HorizonBadge({
  horizon,
}: {
  horizon: "long_term" | "short_term" | "next_action";
}) {
  if (horizon === "next_action") {
    return (
      <Badge variant="accent" size="sm">
        next
      </Badge>
    );
  }
  if (horizon === "short_term") {
    return (
      <Badge variant="info" size="sm">
        short
      </Badge>
    );
  }
  return (
    <Badge variant="neutral" size="sm">
      long
    </Badge>
  );
}
