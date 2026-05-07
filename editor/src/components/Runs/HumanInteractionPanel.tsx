import { useEffect, useMemo, useState } from "react";

import { resumeRun } from "@/api/runs";
import { Badge, Button } from "@/components/ui";
import { useHumanNodeSchema } from "@/hooks/useHumanNodeSchema";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

import HumanInteractionForm from "./HumanInteractionForm";
import PauseForm from "./PauseForm";

interface Props {
  runId: string;
}

export default function HumanInteractionPanel({ runId }: Props) {
  const status = useRunStore((s) => s.snapshot?.run.status);
  const checkpoint = useRunStore((s) => s.snapshot?.run.checkpoint);
  const pending = useRunStore((s) => s.pendingHumanInput);
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const currentSource = useDocumentStore((s) => s.currentSource);

  const { fields, loading, staleHash } = useHumanNodeSchema(runId, pending?.node_id);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showContext, setShowContext] = useState(false);
  // Local guard: once a submit succeeds, keep the panel hidden even if
  // a stale snapshot or a delayed checkpoint fetch briefly re-asserts
  // pendingHumanInput before the WS run_resumed event lands.
  const [submitted, setSubmitted] = useState(false);
  const interactionId = pending?.interaction_id;
  useEffect(() => {
    if (interactionId) setSubmitted(false);
  }, [interactionId]);

  const contextOutputs = useMemo(
    () => extractOutputs(checkpoint),
    [checkpoint],
  );

  if (status !== "paused_waiting_human" || !pending || submitted) {
    return null;
  }

  const submit = async (answers: Record<string, unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await resumeRun(runId, {
        answers,
        source: currentSource ?? undefined,
      });
      setSubmitted(true);
      setRunStatus("running");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const useFallback = !loading && (fields === null || fields.length === 0);
  const approveField = fields?.find(
    (f) => f.type === "bool" && f.name === "approved",
  );

  return (
    <div className="fixed bottom-0 left-0 right-0 z-40 border-t-2 border-warning shadow-2xl bg-surface-1 max-h-[60vh] overflow-y-auto">
      <div className="mx-auto max-w-3xl px-4 py-3 space-y-3">
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

        {contextOutputs && (
          <details
            open={showContext}
            onToggle={(e) => setShowContext((e.target as HTMLDetailsElement).open)}
            className="rounded border border-border-subtle bg-surface-0"
          >
            <summary className="cursor-pointer px-2 py-1.5 text-[11px] font-medium text-fg-default select-none">
              Context (outputs from previous nodes)
            </summary>
            <pre className="px-2 pb-2 text-[10px] font-mono text-fg-muted whitespace-pre-wrap break-all max-h-64 overflow-y-auto">
              {contextOutputs}
            </pre>
          </details>
        )}

        {loading ? (
          <p className="text-[11px] text-fg-subtle">Loading schema…</p>
        ) : useFallback ? (
          <PauseForm
            runId={runId}
            questions={pending.questions ?? {}}
            onSubmitted={() => {
              setSubmitted(true);
              setRunStatus("running");
            }}
          />
        ) : (
          <>
            <HumanInteractionForm
              fields={fields!}
              questions={pending.questions ?? {}}
              busy={busy}
              errorMessage={error}
              onSubmit={(a) => void submit(a)}
            />
            {approveField && (
              <ApproveRejectActions
                approveFieldName={approveField.name}
                busy={busy}
                onSubmit={submit}
              />
            )}
          </>
        )}
      </div>
    </div>
  );
}

interface ApproveRejectActionsProps {
  approveFieldName: string;
  busy: boolean;
  onSubmit: (answers: Record<string, unknown>) => void;
}

function ApproveRejectActions({
  approveFieldName,
  busy,
  onSubmit,
}: ApproveRejectActionsProps) {
  // Quick-action shortcuts: send only the approved bool, ignoring
  // any comments the user typed in the form. To attach comments,
  // users tick the form's checkbox and click the form's Submit.
  return (
    <div className="flex items-center gap-2 pt-2 border-t border-border-subtle">
      <span className="text-[10px] text-fg-subtle">Quick action (no comments):</span>
      <Button
        variant="primary"
        size="sm"
        disabled={busy}
        onClick={() => onSubmit({ [approveFieldName]: true })}
      >
        {busy ? "…" : "Approve"}
      </Button>
      <Button
        variant="danger"
        size="sm"
        disabled={busy}
        onClick={() => onSubmit({ [approveFieldName]: false })}
      >
        {busy ? "…" : "Reject"}
      </Button>
    </div>
  );
}

function extractOutputs(checkpoint: unknown): string | null {
  if (!checkpoint || typeof checkpoint !== "object") return null;
  const cp = checkpoint as { outputs?: Record<string, unknown> };
  if (!cp.outputs || Object.keys(cp.outputs).length === 0) return null;
  try {
    return JSON.stringify(cp.outputs, null, 2);
  } catch {
    return null;
  }
}
