import { useEffect, useState } from "react";

import { resumeRun } from "@/api/runs";
import type { RunHeader } from "@/api/runs";
import { Button, Dialog } from "@/components/ui";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";
import { useUIStore } from "@/store/ui";

interface Props {
  run: RunHeader;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// ResumeDialog is the single entry point for resuming a failed_resumable
// or cancelled run from the header. It bundles the plain "resume" and
// "resume --force" gestures into one modal with a checkbox so the user
// doesn't have to guess which button does what.
export default function ResumeDialog({ run, open, onOpenChange }: Props) {
  const [force, setForce] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const addToast = useUIStore((s) => s.addToast);
  // Cloud mode requires the .iter source inline; the document store
  // caches it on openFile/saveFile. Local mode ignores `source` and
  // resolves the file via the run's persisted FilePath.
  const currentSource = useDocumentStore((s) => s.currentSource);

  // Reset transient state when the dialog re-opens for a different run
  // or after a previous attempt: force defaults to off, and stale errors
  // shouldn't carry over from a closed session.
  useEffect(() => {
    if (open) {
      setForce(false);
      setError(null);
      setBusy(false);
    }
  }, [open, run.id]);

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await resumeRun(run.id, { force, source: currentSource ?? undefined });
      setRunStatus("running");
      // The broker dropped this run's subscribers when the prior pass
      // hit terminal status; without a fresh dial the resumed engine
      // publishes into the void and the UI sees no events until the
      // user reloads the page.
      requestWsReconnect();
      addToast(
        force ? "Resume requested (force)" : "Resume requested",
        "info",
      );
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const statusLabel =
    run.status === "failed_resumable" ? "failed (resumable)" : run.status;

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (busy) return;
        onOpenChange(o);
      }}
      title="Resume run"
      description={
        <span>
          <span className="font-mono">{run.id}</span>
          {" — "}
          {statusLabel}
        </span>
      }
      widthClass="max-w-md"
      footer={
        <>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => onOpenChange(false)}
            disabled={busy}
          >
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            onClick={() => void submit()}
            loading={busy}
            disabled={busy}
          >
            Resume
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-sm">
        <p className="text-fg-muted">
          Restart this run from its last checkpoint. The engine reuses the
          {" "}
          <span className="font-mono">.iter</span> file persisted at launch.
        </p>
        <label className="flex items-start gap-2 cursor-pointer select-none">
          <input
            type="checkbox"
            checked={force}
            onChange={(e) => setForce(e.target.checked)}
            disabled={busy}
            className="mt-0.5 accent-accent"
          />
          <span className="flex flex-col">
            <span className="font-medium">Force</span>
            <span className="text-[11px] text-fg-subtle">
              Allow resume even when the workflow file changed since launch
              ({" "}
              <span className="font-mono">--force</span>). Use after fixing a
              bug in the <span className="font-mono">.iter</span> source.
            </span>
          </span>
        </label>
        {error && (
          <div className="text-xs text-danger break-words">{error}</div>
        )}
      </div>
    </Dialog>
  );
}
