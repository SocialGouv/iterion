import { useState } from "react";

import { resumeRun } from "@/api/runs";
import { Badge } from "@/components/ui";
import { useHumanNodeSchema } from "@/hooks/useHumanNodeSchema";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

import HumanInteractionForm from "./HumanInteractionForm";
import PauseForm from "./PauseForm";

interface Props {
  runId: string;
}

/** Top-level "Répondre" panel for paused runs.
 *
 *  Renders only when the run is paused waiting for human input.
 *  Reads `pendingHumanInput` from the store (live: from the
 *  `human_input_requested` event; reload: rehydrated from
 *  RunHeader.checkpoint.interaction_questions).
 *
 *  Submission posts to POST /api/runs/{id}/resume (HTTP). The WS
 *  `run_resumed` event then flips the run status, which makes the
 *  panel unmount automatically — no explicit teardown here. */
export default function HumanInteractionPanel({ runId }: Props) {
  const status = useRunStore((s) => s.snapshot?.run.status);
  const pending = useRunStore((s) => s.pendingHumanInput);
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const currentSource = useDocumentStore((s) => s.currentSource);

  const { fields, staleHash } = useHumanNodeSchema(runId, pending?.node_id);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (status !== "paused_waiting_human" || !pending) {
    return null;
  }

  const onSubmit = async (answers: Record<string, unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await resumeRun(runId, {
        answers,
        source: currentSource ?? undefined,
      });
      // Optimistic flip — the WS run_resumed event will confirm and
      // also clear pendingHumanInput, unmounting this panel.
      setRunStatus("running");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // Free-text fallback: a Human node without a declared output schema
  // (or whose schema couldn't be loaded — e.g. stale workflow file)
  // gets the legacy textarea-per-question form. Preserves
  // backward-compat with runs paused before this panel was built.
  const useFallback = fields === null || fields.length === 0;

  return (
    <div className="border-y border-border-subtle bg-surface-1 px-4 py-3">
      <div className="mx-auto max-w-3xl space-y-2">
        <div className="flex items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <Badge variant="warning">Awaiting input</Badge>
            {pending.node_id && (
              <span className="text-[11px] font-mono text-fg-muted">
                node: {pending.node_id}
              </span>
            )}
          </div>
          {staleHash && (
            <span className="text-[10px] text-warning-fg" role="status">
              workflow source changed since launch — submitting may fail
            </span>
          )}
        </div>
        {useFallback ? (
          <PauseForm
            runId={runId}
            questions={pending.questions ?? {}}
            onSubmitted={() => setRunStatus("running")}
          />
        ) : (
          <HumanInteractionForm
            fields={fields}
            questions={pending.questions ?? {}}
            busy={busy}
            errorMessage={error}
            onSubmit={(a) => void onSubmit(a)}
          />
        )}
      </div>
    </div>
  );
}
