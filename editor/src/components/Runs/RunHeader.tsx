import { useState } from "react";
import { useLocation } from "wouter";

import type { RunHeader as RunHeaderType } from "@/api/runs";
import { cancelRun, resumeRun } from "@/api/runs";
import { Button, StatusBadge } from "@/components/ui";

interface Props {
  run: RunHeaderType;
  active: boolean;
  wsState: string;
}

export default function RunHeader({ run, active, wsState }: Props) {
  const [, setLocation] = useLocation();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canCancel = run.status === "running" && active;
  // Resume from header is a "best-effort" trigger — for paused_waiting_human
  // runs the user normally fills the Pause form in the detail panel
  // (Phase 5). The header button stays for failed_resumable / cancelled
  // runs which don't need answers.
  const canResume =
    run.status === "failed_resumable" || run.status === "cancelled";

  const onCancel = async () => {
    setBusy(true);
    setError(null);
    try {
      await cancelRun(run.id);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const onResume = async () => {
    setBusy(true);
    setError(null);
    try {
      await resumeRun(run.id, {});
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <header className="border-b border-border-default px-4 py-2 flex items-center gap-3 text-sm">
      <button
        className="text-xs px-2 py-1 rounded bg-surface-2 hover:bg-surface-3"
        onClick={() => setLocation("/runs")}
      >
        ← Runs
      </button>
      <div className="flex flex-col leading-tight min-w-0 max-w-md">
        <div className="font-medium truncate">
          {run.name || run.workflow_name}
        </div>
        {run.name && (
          <div className="text-[10px] text-fg-subtle truncate">
            {run.workflow_name}
          </div>
        )}
      </div>
      <StatusBadge status={run.status} />
      {active && (
        <span
          className="inline-block w-1.5 h-1.5 rounded-full bg-info animate-pulse"
          title="Run is active in this server process"
        />
      )}
      {error && (
        <span className="text-[10px] text-danger truncate max-w-xs">{error}</span>
      )}
      <div className="ml-auto flex items-center gap-2">
        <span className="text-[10px] text-fg-subtle font-mono">{run.id}</span>
        <span
          className="text-[10px] text-fg-subtle"
          title={`WebSocket: ${wsState}`}
        >
          {wsState}
        </span>
        {canCancel && (
          <Button
            variant="danger"
            size="sm"
            onClick={() => void onCancel()}
            disabled={busy}
          >
            Cancel
          </Button>
        )}
        {canResume && (
          <Button
            variant="primary"
            size="sm"
            onClick={() => void onResume()}
            disabled={busy}
          >
            Resume
          </Button>
        )}
      </div>
    </header>
  );
}
