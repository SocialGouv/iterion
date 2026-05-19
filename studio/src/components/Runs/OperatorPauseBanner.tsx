import { useState } from "react";

import { resumeRun } from "@/api/runs";
import type { RunHeader } from "@/api/runs";
import { Button } from "@/components/ui";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";
import { useUIStore } from "@/store/ui";

interface Props {
  run: RunHeader;
}

// OperatorPauseBanner is rendered between RunMetrics and the
// HumanInteractionPanel when run.status === "paused_operator". Visually
// distinct from the human-input pause (info/cyan instead of amber) and
// offers a single-click Resume that bypasses the workflow-hash check
// (the operator deliberately paused; no .iter edit between
// pause→resume is expected).
export default function OperatorPauseBanner({ run }: Props) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const addToast = useUIStore((s) => s.addToast);
  const currentSource = useDocumentStore((s) => s.currentSource);

  const onResume = async () => {
    setBusy(true);
    setError(null);
    try {
      await resumeRun(run.id, { source: currentSource ?? undefined });
      setRunStatus("running");
      requestWsReconnect();
      addToast("Resume requested", "info");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const checkpointNode = (run.checkpoint as { node_id?: string } | undefined)?.node_id;

  return (
    <div className="shrink-0 px-4 py-2 bg-info-soft/40 border-b border-info/30 flex items-center gap-3 text-[12px]">
      <span className="font-mono text-info">⏸ Paused (operator)</span>
      <span className="text-fg-muted">
        {checkpointNode ? `Agent halted before ${checkpointNode}.` : "Agent halted at the next safe boundary."}
      </span>
      <div className="ml-auto flex items-center gap-2">
        {error && <span className="text-[11px] text-danger">{error}</span>}
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
