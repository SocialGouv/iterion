import { resumeRun } from "@/api/runs";
import type { RunHeader } from "@/api/runs";
import { Button } from "@/components/ui";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";
import { useUIStore } from "@/store/ui";

interface Props {
  run: RunHeader;
}

// OperatorPauseBanner is rendered between RunMetrics and the
// conversation view when run.status === "paused_operator". Visually
// distinct from the human-input pause (info/cyan instead of amber) and
// offers a single-click Resume that bypasses the workflow-hash check
// (the operator deliberately paused; no workflow edit between
// pause→resume is expected).
export default function OperatorPauseBanner({ run }: Props) {
  const { busy, error, run: runAction } = useAsyncAction();
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const addToast = useUIStore((s) => s.addToast);
  const currentSource = useDocumentStore((s) => s.currentSource);

  const onResume = () =>
    runAction(async () => {
      await resumeRun(run.id, { source: currentSource ?? undefined });
      setRunStatus("running");
      requestWsReconnect();
      addToast("Resume requested", "info");
    });

  const checkpointNode = run.checkpoint?.node_id;

  return (
    <div
      className="shrink-0 px-4 py-2 bg-info-soft/40 border-b border-info/30 flex flex-wrap items-center gap-x-3 gap-y-1 text-body"
      role="status"
      aria-live="polite"
    >
      <span className="font-mono text-info">⏸ Paused (operator)</span>
      <span className="text-fg-muted">
        {checkpointNode
          ? `Agent halted before ${checkpointNode}.`
          : "Agent halted at the next safe boundary."}
      </span>
      <span className="text-fg-subtle">
        The run will not progress until you click Resume.
      </span>
      <div className="ml-auto flex items-center gap-2">
        {error && <span className="text-micro text-danger">{error}</span>}
        <Button
          variant="primary"
          size="sm"
          disabled={busy}
          onClick={() => void onResume()}
        >
          {busy ? "…" : "Resume"}
        </Button>
      </div>
    </div>
  );
}
