import { useState } from "react";
import { useLocation } from "wouter";

import type { RunHeader as RunHeaderType, RunStatus } from "@/api/runs";
import { cancelRun, resumeRun } from "@/api/runs";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";

const STATUS_VARIANT: Record<RunStatus, BadgeVariant> = {
  running: "info",
  paused_waiting_human: "warning",
  finished: "success",
  failed: "danger",
  failed_resumable: "danger",
  cancelled: "neutral",
};

export type RunViewMode = "execution" | "workflow";

interface Props {
  run: RunHeaderType;
  active: boolean;
  wsState: string;
  viewMode: RunViewMode;
  onViewModeChange: (m: RunViewMode) => void;
}

export default function RunHeader({
  run,
  active,
  wsState,
  viewMode,
  onViewModeChange,
}: Props) {
  const [, setLocation] = useLocation();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canCancel = run.status === "running" && active;
  const canResume =
    run.status === "paused_waiting_human" ||
    run.status === "failed_resumable" ||
    run.status === "cancelled";

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
      <div className="font-medium truncate max-w-md">{run.workflow_name}</div>
      <Badge variant={STATUS_VARIANT[run.status]}>{labelForStatus(run.status)}</Badge>
      {active && (
        <span
          className="inline-block w-1.5 h-1.5 rounded-full bg-info animate-pulse"
          title="Run is active in this server process"
        />
      )}
      {error && (
        <span className="text-[10px] text-danger truncate max-w-xs">{error}</span>
      )}
      <div
        className="inline-flex items-center rounded border border-border-default overflow-hidden text-[10px] font-medium"
        role="tablist"
        aria-label="Canvas view mode"
      >
        <button
          type="button"
          className={`px-2 py-0.5 ${
            viewMode === "execution"
              ? "bg-accent text-on-accent"
              : "bg-surface-1 text-fg-subtle hover:text-fg-default"
          }`}
          onClick={() => onViewModeChange("execution")}
          title="Show one node per execution (with iterations)"
        >
          Execution
        </button>
        <button
          type="button"
          className={`px-2 py-0.5 ${
            viewMode === "workflow"
              ? "bg-accent text-on-accent"
              : "bg-surface-1 text-fg-subtle hover:text-fg-default"
          }`}
          onClick={() => onViewModeChange("workflow")}
          title="Show the workflow graph with per-node iteration tracking"
        >
          Workflow
        </button>
      </div>
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

function labelForStatus(s: RunStatus): string {
  switch (s) {
    case "paused_waiting_human":
      return "Paused";
    case "failed_resumable":
      return "Failed (resumable)";
    default:
      return s;
  }
}
