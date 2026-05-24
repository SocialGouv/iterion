import { useCallback, useEffect, useState } from "react";

import * as dispatcher from "@/api/dispatcher";
import {
  dispatcherPillMeta,
  type DispatcherPillState,
} from "@/components/shared/dispatcherPillMeta";

interface Props {
  /** Called when the operator clicks the "Settings" button. */
  onOpenSettings: () => void;
  /** Optional override for the polling cadence; defaults to 2s. */
  pollIntervalMs?: number;
}

// DispatcherControlBar polls /api/v1/dispatcher/status and surfaces a
// state pill + the appropriate lifecycle buttons. Lives at the top of
// the Board + Dispatcher views so the operator can pilot the daemon
// from either one.
export default function DispatcherControlBar({ onOpenSettings, pollIntervalMs = 2000 }: Props) {
  const [status, setStatus] = useState<dispatcher.ManagerStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const s = await dispatcher.getStatus();
      setStatus(s);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
    const t = setInterval(() => void refresh(), pollIntervalMs);
    return () => clearInterval(t);
  }, [refresh, pollIntervalMs]);

  const guard = useCallback(
    async (op: () => Promise<dispatcher.ManagerStatus>) => {
      setBusy(true);
      try {
        const s = await op();
        setStatus(s);
        setError(null);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(false);
      }
    },
    [],
  );

  const pill = renderPill(status, error);
  const state = status?.state ?? "idle";
  const hasConfig = status?.has_config ?? false;

  return (
    <div className="flex items-center gap-2 border-b border-border-default bg-surface-1 px-4 py-2 text-xs">
      {pill}
      {status?.last_error && (
        <span className="ml-2 max-w-[400px] truncate text-red-300" title={status.last_error}>
          ⚠ {status.last_error}
        </span>
      )}
      <div className="ml-auto flex items-center gap-2">
        {(state === "idle" || state === "error") && (
          <button
            className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:bg-transparent"
            disabled={busy || !hasConfig}
            title={hasConfig ? "Start the dispatcher" : "Save a config in Settings first"}
            onClick={() => void guard(dispatcher.start)}
          >
            ▶ Start
          </button>
        )}
        {state === "running" && (
          <>
            <button
              className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:bg-transparent"
              disabled={busy}
              onClick={() => void guard(dispatcher.pause)}
            >
              ⏸ Pause
            </button>
            <button
              className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:bg-transparent"
              disabled={busy}
              onClick={() => void guard(dispatcher.stop)}
            >
              ■ Stop
            </button>
          </>
        )}
        {state === "paused" && (
          <>
            <button
              className="rounded bg-accent px-2 py-1 text-fg-onAccent hover:opacity-90 disabled:opacity-50 disabled:cursor-not-allowed"
              disabled={busy}
              onClick={() => void guard(dispatcher.resume)}
            >
              ▶ Resume
            </button>
            <button
              className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:bg-transparent"
              disabled={busy}
              onClick={() => void guard(dispatcher.stop)}
            >
              ■ Stop
            </button>
          </>
        )}
        <button
          className="rounded border border-border-default px-2 py-1 hover:bg-surface-2"
          onClick={onOpenSettings}
        >
          ⚙ Settings
        </button>
      </div>
    </div>
  );
}

function renderPill(status: dispatcher.ManagerStatus | null, fetchError: string | null) {
  const pillState: DispatcherPillState =
    fetchError && !status ? "unreachable" : status?.state ?? "idle";
  const meta = dispatcherPillMeta(pillState);
  // Append the uptime anchor whenever the daemon reports a start time —
  // the server keeps started_at set across pause/error, not just while
  // running, so this matches the pre-refactor pill which showed
  // "since …" in every state that had one.
  const title = status?.started_at
    ? `${meta.title} · since ${status.started_at}`
    : meta.title;
  return (
    <span
      className={`rounded px-2 py-0.5 font-medium ${meta.className}`}
      title={title}
    >
      {meta.label}
    </span>
  );
}
