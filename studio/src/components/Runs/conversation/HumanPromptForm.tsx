import { useEffect, useMemo, useRef, useState } from "react";

import { getRun, resumeRun } from "@/api/runs";
import { Button } from "@/components/ui/Button";
import { WizardForm } from "@/components/ui/WizardForm";
import { useHumanNodeSchema } from "@/hooks/useHumanNodeSchema";
import type { FormAnswer } from "@/lib/whats-next/questionForm";
import {
  coerceFormAnswerToSchema,
  formSpecFromSchema,
} from "@/lib/forms/formSpecFromSchema";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

import PauseForm from "../PauseForm";

interface Props {
  runId: string;
  nodeId: string;
  questions: Record<string, unknown>;
  // Quick-action chips (skip / idk) the operator can pick instead of
  // typing a reply. Only meaningful on free-text-only turns. Default
  // = ["skip", "idk"]; pass empty to suppress.
  quickActions?: ReadonlyArray<"skip" | "idk" | "later">;
}

// HumanPromptForm renders the inline form for a pending human-pause
// turn.
//
// Pause-and-resume contract: after resumeRun the broker has already
// dropped this run's subscribers (they were torn down when the run
// hit `paused_waiting_human`). The engine publishes the resumed node
// updates into a void unless the client dials a fresh WS — without
// `requestWsReconnect`, the canvas stays frozen until reload. The
// 600ms `getRun` fallback covers very short runs (resume → done in
// <2s) that finish before the WS redial completes. Both pieces, plus
// the snapshotTimerRef cleanup on unmount, are load-bearing.
export default function HumanPromptForm({
  runId,
  nodeId,
  questions,
  quickActions = ["skip", "idk"],
}: Props) {
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const currentSource = useDocumentStore((s) => s.currentSource);

  const { fields, loading, staleHash } = useHumanNodeSchema(runId, nodeId);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState(false);
  // The latest form draft is captured here so the quick-action
  // Approve/Reject and skip/idk buttons can submit alongside the
  // current text. WizardForm emits FormAnswer atomically; we also
  // expose an onChange via a controlled draft pattern.
  const [latestAnswer, setLatestAnswer] = useState<FormAnswer>({});

  // Belt-and-braces post-resume snapshot fetch lives on this timer
  // ref so a panel torn down within the 600ms window doesn't have
  // its applySnapshot fire against another run.
  const snapshotTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    setSubmitted(false);
    setLatestAnswer({});
    setError(null);
  }, [nodeId]);
  useEffect(() => {
    return () => {
      if (snapshotTimerRef.current != null) {
        clearTimeout(snapshotTimerRef.current);
        snapshotTimerRef.current = null;
      }
    };
  }, []);

  // Compute formSpec unconditionally — useMemo MUST be called before any
  // early return so the hook order stays stable across renders.
  // Placing the hook after the `if (submitted) return null` branch would
  // crash with React error #310 ("Rendered more hooks than during the
  // previous render") the moment the form toggles between rendered and
  // null on a status transition.
  const formSpec = useMemo(() => {
    if (!fields || fields.length === 0) return null;
    const approve = fields.find(
      (f) => f.type === "bool" && f.name === "approved",
    );
    const visible = approve
      ? fields.filter((f) => f.name !== approve.name)
      : fields;
    if (visible.length === 0) return null;
    // Approve/Reject buttons live OUTSIDE the wizard at this layout
    // level. Paginating the remaining questions would force the
    // operator to step through a wizard to reach the approve action;
    // collapsing them onto one page keeps every input (feedback +
    // any per-item selector like whats-next's `selected_titles`)
    // visible alongside the verdict buttons.
    const mode = approve ? "flat" : undefined;
    return formSpecFromSchema(visible, questions, {
      submitLabel: "Submit & Resume",
      mode,
    });
  }, [fields, questions]);

  if (submitted) return null;

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
      // stays frozen until the user reloads.
      requestWsReconnect();
      // Belt-and-braces: fetch a REST snapshot ~600ms later so a
      // short-lived run (resume → done in <2s) that finishes before
      // the WS redial completes still surfaces in the canvas. The WS
      // tail catches up afterwards for longer-running runs.
      if (snapshotTimerRef.current != null) {
        clearTimeout(snapshotTimerRef.current);
      }
      snapshotTimerRef.current = setTimeout(() => {
        snapshotTimerRef.current = null;
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
  const visibleFields = approveField
    ? (fields ?? []).filter((f) => f.name !== approveField.name)
    : fields ?? [];

  const submitWithApproved = (approved: boolean) => {
    if (!fields) return;
    const { answers, errors } = coerceFormAnswerToSchema(
      visibleFields,
      latestAnswer,
    );
    if (Object.keys(errors).length > 0) {
      setError("Fix invalid fields: " + Object.keys(errors).join(", "));
      return;
    }
    void submit({ ...answers, [approveField!.name]: approved });
  };

  const submitFromWizard = (formAnswer: FormAnswer) => {
    if (!fields) return;
    const { answers, errors } = coerceFormAnswerToSchema(
      visibleFields,
      formAnswer,
    );
    if (Object.keys(errors).length > 0) {
      setError("Fix invalid fields: " + Object.keys(errors).join(", "));
      return;
    }
    void submit(answers);
  };

  // Quick-action submit — short-circuit the form and resume with a
  // sentinel token the bot prompt is expected to recognise
  // (`[QA:skip]` / `[QA:idk]`). The token lands in the first string
  // field of the answers map; we don't try to pick "the" field — most
  // human nodes have one string slot and a typed value works fine
  // there for the bot's prompt-side parsing.
  const submitQuickAction = (action: "skip" | "idk" | "later") => {
    if (!fields || fields.length === 0) {
      // No schema → resume with the token as a single "text" key.
      void submit({ text: `[QA:${action}]` });
      return;
    }
    const stringField = fields.find((f) => f.type === "string");
    if (!stringField) {
      // No string slot to take the token; fall back to a generic key.
      void submit({ text: `[QA:${action}]` });
      return;
    }
    void submit({ [stringField.name]: `[QA:${action}]` });
  };

  const showQuickActions = !approveField && quickActions.length > 0;

  return (
    <div className="space-y-2">
      {staleHash && (
        <div className="text-[10px] text-warning-fg" role="status">
          The workflow source changed since this run started. Submit will still
          try — pass <code>--force</code> later if it rejects.
        </div>
      )}
      {loading ? (
        <p className="text-[11px] text-fg-subtle">Loading question form…</p>
      ) : useFallback ? (
        <PauseForm
          runId={runId}
          questions={questions}
          onSubmitted={() => {
            setSubmitted(true);
            setRunStatus("running");
          }}
        />
      ) : (
        <>
          {formSpec && (
            <WizardForm
              spec={formSpec}
              busy={busy}
              hideSubmit={!!approveField}
              onAnswerChange={setLatestAnswer}
              onSubmit={(answer) => {
                setLatestAnswer(answer);
                if (!approveField) submitFromWizard(answer);
              }}
            />
          )}
          {error && (
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
          {showQuickActions && (
            <div className="flex items-center gap-2 pt-1">
              <span className="text-[10px] text-fg-subtle">Quick reply</span>
              {quickActions.map((qa) => (
                <button
                  key={qa}
                  type="button"
                  disabled={busy}
                  onClick={() => submitQuickAction(qa)}
                  className="px-2 py-0.5 rounded-full border border-border-subtle text-[11px] text-fg-muted hover:text-fg-default hover:border-border-strong disabled:opacity-50"
                  title={quickActionTitle(qa)}
                >
                  {labelFor(qa)}
                </button>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}

function labelFor(qa: "skip" | "idk" | "later"): string {
  if (qa === "skip") return "Skip";
  if (qa === "idk") return "I don't know";
  return "Later";
}

// quickActionTitle returns the hover hint for the quick-reply chips —
// plain English instead of the raw `[QA:*]` marker the bot consumes
// downstream.
function quickActionTitle(qa: "skip" | "idk" | "later"): string {
  switch (qa) {
    case "skip":
      return "Submit a skip token; the bot will route accordingly.";
    case "idk":
      return "Tell the bot you don't know; it can decide how to proceed.";
    case "later":
      return "Ask the bot to come back to this question later.";
  }
}
