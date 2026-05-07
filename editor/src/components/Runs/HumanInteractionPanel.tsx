import { useEffect, useMemo, useState, type ReactNode } from "react";

import { getRun, resumeRun } from "@/api/runs";
import { Button } from "@/components/ui";
import { useHumanNodeSchema } from "@/hooks/useHumanNodeSchema";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

import HumanInteractionForm, {
  buildInitialDrafts,
  coerceDrafts,
} from "./HumanInteractionForm";
import PauseForm from "./PauseForm";

interface Props {
  runId: string;
}

export default function HumanInteractionPanel({ runId }: Props) {
  const status = useRunStore((s) => s.snapshot?.run.status);
  const checkpoint = useRunStore((s) => s.snapshot?.run.checkpoint);
  const pending = useRunStore((s) => s.pendingHumanInput);
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const currentSource = useDocumentStore((s) => s.currentSource);

  const { fields, loading, staleHash } = useHumanNodeSchema(runId, pending?.node_id);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState(false);
  const interactionId = pending?.interaction_id;
  useEffect(() => {
    if (interactionId) setSubmitted(false);
  }, [interactionId]);

  // drafts live up here so the quick-action buttons (Approve / Reject)
  // can read the user's comments even though the form internally just
  // reflects them.
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  useEffect(() => {
    if (fields) setDrafts(buildInitialDrafts(fields));
  }, [fields, interactionId]);
  const setDraft = (name: string, next: string) =>
    setDrafts((prev) => ({ ...prev, [name]: next }));

  const review = useMemo(() => extractReview(checkpoint), [checkpoint]);

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
      // The broker dropped this run's subscribers when the prior pass
      // hit paused_waiting_human; without a fresh dial the resumed
      // engine publishes node updates into the void and the canvas
      // stays frozen until the user reloads. Mirrors ResumeDialog.tsx.
      requestWsReconnect();
      // Belt-and-braces: fetch a REST snapshot ~600ms later so a
      // short-lived run (resume → done in <2s) that finishes before
      // the WS redial completes still surfaces in the canvas. The WS
      // tail catches up afterwards for longer-running runs.
      window.setTimeout(() => {
        getRun(runId)
          .then(applySnapshot)
          .catch(() => {});
      }, 600);
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
  // When we have a typed approved bool, drive submission entirely from
  // the Approve/Reject buttons and hide the redundant checkbox row.
  const visibleFields = approveField
    ? (fields ?? []).filter((f) => f.name !== approveField.name)
    : fields ?? [];

  const submitWithApproved = (approved: boolean) => {
    if (!fields) return;
    const { answers, errors } = coerceDrafts(visibleFields, drafts);
    if (Object.keys(errors).length > 0) {
      setError(
        "Fix invalid fields: " + Object.keys(errors).join(", "),
      );
      return;
    }
    void submit({ ...answers, [approveField!.name]: approved });
  };

  const submitForm = () => {
    if (!fields) return;
    const { answers, errors } = coerceDrafts(fields, drafts);
    if (Object.keys(errors).length > 0) {
      setError("Fix invalid fields: " + Object.keys(errors).join(", "));
      return;
    }
    void submit(answers);
  };

  return (
    <div className="fixed bottom-0 left-0 right-0 z-40 border-t-2 border-warning shadow-2xl bg-surface-1 max-h-[60vh] overflow-y-auto">
      <div className="mx-auto max-w-3xl px-4 py-3 space-y-3">
        {staleHash && (
          <div className="text-[10px] text-warning-fg" role="status">
            workflow source changed since launch — submitting may fail
          </div>
        )}

        <ReviewBlock questions={pending.questions ?? {}} runOutputs={review} />

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
            {visibleFields.length > 0 && (
              <HumanInteractionForm
                fields={visibleFields}
                questions={pending.questions ?? {}}
                drafts={drafts}
                onDraftChange={setDraft}
                busy={busy}
                errorMessage={error}
                onSubmit={approveField ? undefined : submitForm}
              />
            )}
            {error && !visibleFields.length && (
              <p className="text-danger-fg text-[11px]" role="alert">
                {error}
              </p>
            )}
            {approveField && (
              <div className="flex items-center gap-2 pt-2 border-t border-border-subtle">
                <Button
                  variant="primary"
                  size="sm"
                  disabled={busy}
                  onClick={() => submitWithApproved(true)}
                >
                  {busy ? "…" : "Approve"}
                </Button>
                <Button
                  variant="danger"
                  size="sm"
                  disabled={busy}
                  onClick={() => submitWithApproved(false)}
                >
                  {busy ? "…" : "Reject"}
                </Button>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

interface ReviewBlockProps {
  // questions is the human node's resolved input (via {{outputs.X}}
  // references in the inbound edges' `with {}` clauses). For
  // `planner -> approval with { plan: "{{outputs.planner}}" }`, this
  // is { plan: <full planner output> } — i.e. the actual content
  // the operator must accept or reject. Surfacing it inline is the
  // whole point of the panel.
  questions: Record<string, unknown>;
  // runOutputs is the full checkpoint.outputs map (every prior node's
  // structured output). Useful as broader context when the inbound
  // edge mapping doesn't bring everything; tucked behind a <details>.
  runOutputs: Record<string, Record<string, unknown>> | null;
}

function ReviewBlock({ questions, runOutputs }: ReviewBlockProps) {
  const filtered = Object.fromEntries(
    Object.entries(questions).filter(
      ([k, v]) => v !== undefined && !k.startsWith("_"),
    ),
  );
  const summary = summarizeForReview(filtered);
  const hasFiltered = Object.keys(filtered).length > 0;
  if (!hasFiltered && !runOutputs) return null;

  return (
    <div className="space-y-2">
      {summary.objective && (
        <div className="text-[12px]">
          <span className="text-[10px] uppercase tracking-wide text-fg-subtle mr-1">
            Objective
          </span>
          <span className="text-fg-default">{summary.objective}</span>
        </div>
      )}
      {summary.verdict && (
        <div className="text-[12px] flex items-baseline gap-2">
          <span className="text-[10px] uppercase tracking-wide text-fg-subtle">
            Verdict
          </span>
          <span
            className={
              summary.verdict.approved
                ? "text-success-fg font-medium"
                : "text-danger-fg font-medium"
            }
          >
            {summary.verdict.approved ? "✓ approved" : "✗ rejected"}
          </span>
          {summary.verdict.confidence && (
            <span className="text-fg-muted">
              · confidence {summary.verdict.confidence}
            </span>
          )}
          {summary.verdict.uiQuality && (
            <span className="text-fg-muted">· {summary.verdict.uiQuality}</span>
          )}
        </div>
      )}
      {summary.blockers && summary.blockers.length > 0 && (
        <ul className="text-[11px] text-danger-fg list-disc ml-5 space-y-0.5">
          {summary.blockers.map((b, i) => (
            <li key={i}>{b}</li>
          ))}
        </ul>
      )}
      {(summary.hasMore || runOutputs) && (
        <details className="rounded border border-border-subtle bg-surface-0">
          <summary className="cursor-pointer px-2 py-1 text-[10px] text-fg-subtle select-none">
            Show full context
          </summary>
          <div className="px-2 pb-2 space-y-2">
            {summary.hasMore && (
              <div className="space-y-1">
                {Object.entries(filtered)
                  .filter(([k]) => !summary.handled.has(k))
                  .flatMap(([k, v]) => flattenForDisplay(k, v))}
              </div>
            )}
            {runOutputs && (
              <div className="pt-2 border-t border-border-subtle space-y-1">
                <div className="text-[10px] uppercase tracking-wide text-fg-subtle">
                  All run outputs
                </div>
                {Object.entries(runOutputs).map(([nodeId, fields]) => (
                  <div key={nodeId} className="text-[11px]">
                    <div className="text-[10px] font-mono text-fg-muted">
                      from {nodeId}
                    </div>
                    {Object.entries(fields).map(([k, v]) => (
                      <div key={k}>
                        <span className="font-mono text-fg-subtle">{k}: </span>
                        <span className="whitespace-pre-wrap break-words">
                          {renderValue(v)}
                        </span>
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            )}
          </div>
        </details>
      )}
    </div>
  );
}

interface ReviewSummary {
  objective?: string;
  verdict?: {
    approved: boolean;
    confidence?: string;
    uiQuality?: string;
  };
  blockers?: string[];
  handled: Set<string>;
  hasMore: boolean;
}

// summarizeForReview extracts the few highlights an operator actually
// needs to decide: a one-line objective, a one-line verdict (approved
// + confidence + ui_quality), and the list of blockers if rejected.
// Anything else (full plan, full observation, additional_instructions)
// stays behind the "Show full context" details. Heuristics keyed off
// conventional field names — workflows that don't follow them fall
// back to the generic flatten path.
function summarizeForReview(
  questions: Record<string, unknown>,
): ReviewSummary {
  const handled = new Set<string>();
  const out: ReviewSummary = { handled, hasMore: false };

  if (typeof questions.objective === "string") {
    out.objective = questions.objective;
    handled.add("objective");
  }

  const verdict = questions.verdict;
  if (isPlainObject(verdict)) {
    const v = stripMeta(verdict) as Record<string, unknown>;
    if (typeof v.approved === "boolean") {
      out.verdict = {
        approved: v.approved,
        confidence: typeof v.confidence === "string" ? v.confidence : undefined,
        uiQuality:
          typeof v.ui_quality === "string" ? v.ui_quality : undefined,
      };
      if (Array.isArray(v.blockers) && v.blockers.length > 0) {
        out.blockers = v.blockers.filter(
          (b): b is string => typeof b === "string",
        );
      }
      handled.add("verdict");
    }
  }

  for (const k of Object.keys(questions)) {
    if (!handled.has(k)) {
      out.hasMore = true;
      break;
    }
  }
  return out;
}

function renderValue(v: unknown): string {
  if (typeof v === "string") return v;
  if (v == null) return "—";
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

// flattenForDisplay turns one (key, value) pair from the review map
// into one or more rendered rows, hiding `_*` metadata keys (added
// by the iterion runtime: _backend, _cost_usd, _model, _tokens, …)
// and unwrapping single-key objects so the operator sees the actual
// content rather than nested braces. For multi-field objects, each
// non-meta field becomes a labelled row.
function flattenForDisplay(label: string, value: unknown): ReactNode[] {
  const stripped = stripMeta(value);
  if (stripped === undefined) return [];
  if (isPlainObject(stripped)) {
    const inner = Object.entries(stripped);
    if (inner.length === 0) return [];
    return inner.map(([k, v]) => (
      <ReviewRow key={`${label}.${k}`} label={k} value={v} />
    ));
  }
  return [<ReviewRow key={label} label={label} value={stripped} />];
}

function ReviewRow({ label, value }: { label: string; value: unknown }) {
  return (
    <div className="text-[12px]">
      <div className="text-[10px] font-mono text-fg-subtle mb-0.5">{label}</div>
      <div className="whitespace-pre-wrap break-words">{renderValue(value)}</div>
    </div>
  );
}

function stripMeta(v: unknown): unknown {
  if (!isPlainObject(v)) return v;
  const out: Record<string, unknown> = {};
  for (const [k, val] of Object.entries(v)) {
    if (!k.startsWith("_") && val !== undefined) out[k] = val;
  }
  return out;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// extractReview returns checkpoint.outputs as a typed map. Returns
// null when there's nothing to review (no outputs accumulated yet).
function extractReview(
  checkpoint: unknown,
): Record<string, Record<string, unknown>> | null {
  if (!checkpoint || typeof checkpoint !== "object") return null;
  const cp = checkpoint as { outputs?: Record<string, Record<string, unknown>> };
  if (!cp.outputs || Object.keys(cp.outputs).length === 0) return null;
  return cp.outputs;
}
