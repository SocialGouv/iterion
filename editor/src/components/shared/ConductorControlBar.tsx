import { useCallback, useEffect, useState } from "react";

import * as conductor from "@/api/conductor";

interface Props {
  /** Called when the operator clicks the "Settings" button. */
  onOpenSettings: () => void;
  /** Optional override for the polling cadence; defaults to 2s. */
  pollIntervalMs?: number;
}

// ConductorControlBar polls /api/v1/conductor/status and surfaces a
// state pill + the appropriate lifecycle buttons. Lives at the top of
// the Board + Conductor views so the operator can pilot the daemon
// from either one.
export default function ConductorControlBar({ onOpenSettings, pollIntervalMs = 2000 }: Props) {
  const [status, setStatus] = useState<conductor.ManagerStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const s = await conductor.getStatus();
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
    async (op: () => Promise<conductor.ManagerStatus>) => {
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
            className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50"
            disabled={busy || !hasConfig}
            title={hasConfig ? "Start the conductor" : "Save a config in Settings first"}
            onClick={() => void guard(conductor.start)}
          >
            ▶ Start
          </button>
        )}
        {state === "running" && (
          <>
            <button
              className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50"
              disabled={busy}
              onClick={() => void guard(conductor.pause)}
            >
              ⏸ Pause
            </button>
            <button
              className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50"
              disabled={busy}
              onClick={() => void guard(conductor.stop)}
            >
              ■ Stop
            </button>
          </>
        )}
        {state === "paused" && (
          <>
            <button
              className="rounded bg-accent px-2 py-1 text-on-accent hover:opacity-90 disabled:opacity-50"
              disabled={busy}
              onClick={() => void guard(conductor.resume)}
            >
              ▶ Resume
            </button>
            <button
              className="rounded border border-border-default px-2 py-1 hover:bg-surface-2 disabled:opacity-50"
              disabled={busy}
              onClick={() => void guard(conductor.stop)}
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

function renderPill(status: conductor.ManagerStatus | null, fetchError: string | null) {
  if (fetchError && !status) {
    return <span className="rounded bg-red-500/20 px-2 py-0.5 text-red-200">unreachable</span>;
  }
  const state = status?.state ?? "idle";
  const cls = {
    idle: "bg-fg-muted/20 text-fg-muted",
    running: "bg-green-500/20 text-green-300",
    paused: "bg-amber-500/20 text-amber-300",
    error: "bg-red-500/20 text-red-300",
  }[state] ?? "bg-fg-muted/20 text-fg-muted";
  return (
    <span className={`rounded px-2 py-0.5 font-medium ${cls}`} title={status?.started_at ? `since ${status.started_at}` : undefined}>
      conductor: {state}
    </span>
  );
}
