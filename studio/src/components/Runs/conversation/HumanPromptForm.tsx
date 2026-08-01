import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useRef, useState } from "react";

import {
  getRun,
  isWorkflowSourceChangedError,
  resumeRun,
} from "@/api/runs";
import { Button } from "@/components/ui/Button";
import {
  isQuestionValid,
  WizardForm,
} from "@/components/ui/WizardForm";
import { useHumanNodeSchema } from "@/hooks/useHumanNodeSchema";
import { ASK_USER_RESPONSE_KEY } from "@/lib/askUserOptions";
import type {
  FormAnswer,
  FormQuestion,
  FormSpec,
} from "@/lib/whats-next/questionForm";
import {
  coerceFormAnswerToSchema,
  formSpecFromSchema,
} from "@/lib/forms/formSpecFromSchema";
import { useDocumentStore } from "@/store/document";
import { useRunStore } from "@/store/run";

import PauseForm from "../PauseForm";
import GateAttachments from "./GateAttachments";
import GateInboundPayload from "./GateInboundPayload";
import MarkdownText from "./MarkdownText";
import type { GateFileValue } from "@/components/shared/GateFileInput";

interface Props {
  runId: string;
  nodeId: string;
  questions: Record<string, unknown>;
  // Resolved `instructions:` text of the paused node — the author's
  // operator-facing question, rendered as markdown above the form.
  // The run console omits it because HumanQuestionCard already renders
  // the same text as the turn's prompt; board surfaces, which have no
  // conversation around the form, pass it so the operator sees what
  // they are answering instead of a bare input.
  instructions?: string;
  // Quick-action chips (skip / idk) the operator can pick instead of
  // typing a reply. Only meaningful on free-text-only turns. Default
  // = ["skip", "idk"]; pass empty to suppress.
  quickActions?: ReadonlyArray<"skip" | "idk" | "later">;
  // Overrides the workflow source sent with the resume. Run-console
  // callers omit it and the editor buffer (currentSource) is used; a
  // board caller (answering a paused card, not editing this run's
  // workflow) passes `null` to send NO source so the server falls back
  // to the run's persisted FilePath.
  sourceOverride?: string | null;
  // Called after a successful resume INSTEAD of the run-console WS
  // machinery (reconnect / snapshot resync). A board caller passes this
  // to refetch its own view; when omitted the run-console behaviour runs.
  onResumed?: () => void;
}

type ForceRetry =
  | { kind: "form" }
  | {
      kind: "verdict";
      fieldName: string;
      value: boolean | string;
    }
  | {
      kind: "quick-action";
      fieldName: string;
      token: string;
    };

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
  instructions,
  quickActions = ["skip", "idk"],
  sourceOverride,
  onResumed,
}: Props) {
  const setRunStatus = useRunStore((s) => s.setRunStatus);
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const resyncEventsAfterResume = useRunStore(
    (s) => s.resyncEventsAfterResume,
  );
  const currentSource = useDocumentStore((s) => s.currentSource);
  // Explicit prop (incl. null) wins over the editor buffer; null → no
  // source on the resume (server uses the run's persisted FilePath).
  const resolvedSource =
    (sourceOverride !== undefined ? sourceOverride : currentSource) ?? undefined;

  const {
    fields,
    inputFields,
    loading,
    staleHash,
    error: schemaError,
    reload: reloadSchema,
  } = useHumanNodeSchema(runId, nodeId);

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState(false);
  // Intent of the last attempt rejected because the workflow source changed.
  // A force retry rebuilds editable form fields from latestAnswer at click
  // time, while verdict and quick-action intents retain the exact immutable
  // token the operator originally selected.
  const [forceRetry, setForceRetry] = useState<ForceRetry | null>(null);
  // The latest form draft is captured here so the quick-action
  // Approve/Reject and skip/idk buttons can submit alongside the
  // current text. WizardForm emits FormAnswer atomically; we also
  // expose an onChange via a controlled draft pattern.
  const [latestAnswer, setLatestAnswer] = useState<FormAnswer>({});
  // Ad-hoc attachments (the 📎 button), independent of any declared
  // `file` field. Already uploaded to staging by the time they land
  // here — only their ids ride the resume.
  const [adHocFiles, setAdHocFiles] = useState<GateFileValue[]>([]);

  // Belt-and-braces post-resume snapshot fetch lives on this timer
  // ref so a panel torn down within the 600ms window doesn't have
  // its applySnapshot fire against another run.
  const snapshotTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    setSubmitted(false);
    setError(null);
    setForceRetry(null);
  }, [runId, nodeId]);
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
    const action = approve
      ? undefined
      : fields.find(
          (f) =>
            f.name === "action" && (f.enum_values?.length ?? 0) >= 2,
        );
    const verdict = approve ?? action;
    const visible = verdict
      ? fields.filter((f) => f.name !== verdict.name)
      : fields;
    if (visible.length === 0) return null;
    // Verdict buttons (Approve/Reject, or one button per `action` enum
    // value) live OUTSIDE the wizard at this layout level. Paginating
    // the remaining questions would force the operator to step through
    // a wizard to reach the verdict; collapsing them onto one page
    // keeps every input (feedback + any per-item selector like
    // whats-next's `selected_titles`) visible alongside the buttons.
    const mode = verdict ? "flat" : undefined;
    return makeHumanIdentityRequired(
      formSpecFromSchema(visible, questions, {
        submitLabel: "Submit & Resume",
        mode,
      }),
    );
  }, [fields, questions]);

  useEffect(() => {
    if (!formSpec) {
      setLatestAnswer({});
      return;
    }
    setLatestAnswer(defaultAnswerForSpec(formSpec));
  }, [nodeId, formSpec]);

  if (submitted) return null;

  const submit = async (
    answers: Record<string, unknown>,
    force = false,
    retryIntent: ForceRetry = { kind: "form" },
  ) => {
    setBusy(true);
    setError(null);
    setForceRetry(null);
    try {
      await resumeRun(runId, {
        answers,
        source: resolvedSource,
        ...(adHocFiles.length > 0
          ? { attachments: adHocFiles.map((f) => f.uploadId) }
          : {}),
        ...(force ? { force: true } : {}),
      });
      setSubmitted(true);
      // A board caller isn't viewing this run's canvas — skip the
      // run-console WS machinery entirely and let it refetch its view.
      if (onResumed) {
        onResumed();
        return;
      }
      setRunStatus("running");
      // The broker dropped this run's subscribers when the prior pass
      // hit paused_waiting_human; without a fresh dial the resumed
      // engine publishes node updates into the void and the canvas
      // stays frozen until the user reloads.
      requestWsReconnect();
      // Detached event re-sync (store-level, survives this form
      // unmounting when its gate flips to answered). Covers the
      // human-only flow where the resume re-pauses faster than the WS
      // can resubscribe, otherwise dropping the next gate's question
      // event and leaving its form unrendered until a manual reload.
      resyncEventsAfterResume(runId);
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
      const msg = errorMessage(e);
      setError(msg);
      if (isWorkflowSourceChangedError(e)) setForceRetry(retryIntent);
    } finally {
      setBusy(false);
    }
  };

  // An ask_user pause (agent node calling the ask_user tool, possibly
  // with structured options) answers with a single string under
  // `ask_user_response` — the paused node's output_schema describes its
  // eventual structured output, NOT this answer, so schema-driven
  // rendering would show the wrong form. Route straight to the
  // questions-driven PauseForm.
  const isAskUserPause = ASK_USER_RESPONSE_KEY in (questions ?? {});
  // The output-schema fetch FAILED (not merely "no schema"). We must NOT
  // silently drop into the PauseForm fallback: that form renders the
  // node's INPUT questions with no verdict/notes controls, so the
  // operator can neither approve/reject nor record notes — they hit
  // "Resume" (empty answers), the run re-pauses, and the typed feedback
  // is gone. Surface the error + a Retry instead (iterion#244).
  const schemaFailed = !isAskUserPause && !loading && !!schemaError;
  const useFallback =
    isAskUserPause ||
    (!loading && !schemaError && (fields === null || fields.length === 0));
  const approveField = fields?.find(
    (f) => f.type === "bool" && f.name === "approved",
  );
  // A string enum named `action` is the schema convention for a
  // multi-way gate verdict (bmady's approve/expand/revise menus,
  // app-dev's ship/request_changes/hold_for_later). Render one
  // one-click submit button per value — same affordance the bool
  // `approved` convention gets — instead of radio + separate submit.
  const actionField = approveField
    ? undefined
    : fields?.find(
        (f) => f.name === "action" && (f.enum_values?.length ?? 0) >= 2,
      );
  const verdictField = approveField ?? actionField;
  const visibleFields = verdictField
    ? (fields ?? []).filter((f) => f.name !== verdictField.name)
    : fields ?? [];

  const submitWithVerdict = (
    value: boolean | string,
    force = false,
    fieldName = verdictField?.name,
  ) => {
    if (!fields || !fieldName) return;
    const answerWithDefaults = {
      ...defaultAnswerForSpec(formSpec),
      ...latestAnswer,
    };
    const invalidRequired = (formSpec?.questions ?? []).filter(
      (question) =>
        !isQuestionValid(question, answerWithDefaults[question.id]),
    );
    if (invalidRequired.length > 0) {
      setError(
        "Complete required fields: " +
          invalidRequired.map((question) => question.label).join(", "),
      );
      return;
    }
    const { answers, errors } = coerceFormAnswerToSchema(
      visibleFields,
      answerWithDefaults,
    );
    if (Object.keys(errors).length > 0) {
      setError("Fix invalid fields: " + Object.keys(errors).join(", "));
      return;
    }
    void submit(
      { ...answers, [fieldName]: value },
      force,
      { kind: "verdict", fieldName, value },
    );
  };

  const submitFromWizard = (formAnswer: FormAnswer, force = false) => {
    if (!fields) return;
    const { answers, errors } = coerceFormAnswerToSchema(
      visibleFields,
      formAnswer,
    );
    if (Object.keys(errors).length > 0) {
      setError("Fix invalid fields: " + Object.keys(errors).join(", "));
      return;
    }
    void submit(answers, force, { kind: "form" });
  };

  // Quick-action submit — short-circuit the form and resume with a
  // sentinel token the bot prompt is expected to recognise
  // (`[QA:skip]` / `[QA:idk]`). The token lands in the first string
  // field of the answers map; we don't try to pick "the" field — most
  // human nodes have one string slot and a typed value works fine
  // there for the bot's prompt-side parsing.
  const submitQuickAction = (action: "skip" | "idk" | "later") => {
    const token = `[QA:${action}]`;
    if (!fields || fields.length === 0) {
      // No schema → resume with the token as a single "text" key.
      void submit(
        { text: token },
        false,
        { kind: "quick-action", fieldName: "text", token },
      );
      return;
    }
    const stringField = fields.find((f) => f.type === "string");
    if (!stringField) {
      // No string slot to take the token; fall back to a generic key.
      void submit(
        { text: token },
        false,
        { kind: "quick-action", fieldName: "text", token },
      );
      return;
    }
    void submit(
      { [stringField.name]: token },
      false,
      { kind: "quick-action", fieldName: stringField.name, token },
    );
  };

  const retryQuickAction = (
    intent: Extract<ForceRetry, { kind: "quick-action" }>,
  ) => {
    const answerWithDefaults = {
      ...defaultAnswerForSpec(formSpec),
      ...latestAnswer,
    };
    const { answers, errors } = coerceFormAnswerToSchema(
      visibleFields,
      answerWithDefaults,
    );
    if (Object.keys(errors).length > 0) {
      setError("Fix invalid fields: " + Object.keys(errors).join(", "));
      return;
    }
    void submit(
      { ...answers, [intent.fieldName]: intent.token },
      true,
      intent,
    );
  };

  const showQuickActions = !verdictField && quickActions.length > 0;

  return (
    <div className="space-y-2">
      {instructions && (
        <div className="text-body text-fg-default">
          <MarkdownText value={instructions} size="sm" />
        </div>
      )}
      {/*
        The gate's INBOUND payload — the plan / diff / mockup the operator
        is validating (iterion#332). Suppressed on the PauseForm branches:
        an ask_user pause carries the agent's question here, and a
        schema-less gate has PauseForm render the very same map as its
        answerable fields, so showing it twice would be noise.
      */}
      {!isAskUserPause && !useFallback && (
        <GateInboundPayload
          runId={runId}
          questions={questions}
          inputFields={inputFields}
        />
      )}
      {staleHash && (
        <div className="text-caption text-warning-fg" role="status">
          The workflow source changed since this run started. Submit will still
          try — if the server rejects it, a force-retry button will appear.
        </div>
      )}
      {loading && !isAskUserPause ? (
        <p className="text-micro text-fg-subtle">Loading question form…</p>
      ) : schemaFailed ? (
        <div className="space-y-2">
          <p className="text-danger-fg text-micro" role="alert">
            Couldn't load the answer form for this gate: {schemaError}
          </p>
          <p className="text-micro text-fg-subtle">
            The run is still paused — your answer wasn't lost. Retry, or open
            the run console to answer it there.
          </p>
          <Button
            variant="secondary"
            size="sm"
            disabled={loading}
            onClick={() => reloadSchema()}
          >
            Retry
          </Button>
        </div>
      ) : useFallback ? (
        <PauseForm
          runId={runId}
          questions={questions}
          sourceOverride={sourceOverride}
          onSubmitted={() => {
            setSubmitted(true);
            if (onResumed) onResumed();
            else setRunStatus("running");
          }}
        />
      ) : (
        <>
          {formSpec && (
            <WizardForm
              spec={formSpec}
              mode={formSpec.mode}
              busy={busy}
              hideSubmit={!!verdictField}
              onAnswerChange={setLatestAnswer}
              onSubmit={(answer) => {
                setLatestAnswer(answer);
                if (!verdictField) submitFromWizard(answer);
              }}
            />
          )}
          <GateAttachments
            value={adHocFiles}
            onChange={setAdHocFiles}
            disabled={busy}
          />
          {error && (
            <p className="text-danger-fg text-micro" role="alert">
              {error}
            </p>
          )}
          {forceRetry && (
            <div className="flex items-center gap-2">
              <Button
                variant="primary"
                size="sm"
                disabled={busy}
                onClick={() => {
                  if (forceRetry.kind === "verdict") {
                    submitWithVerdict(
                      forceRetry.value,
                      true,
                      forceRetry.fieldName,
                    );
                  } else if (forceRetry.kind === "quick-action") {
                    retryQuickAction(forceRetry);
                  } else {
                    submitFromWizard(latestAnswer, true);
                  }
                }}
              >
                Resume with updated workflow (force)
              </Button>
              <span className="text-micro text-fg-subtle">
                Replays your answer against the current workflow source.
              </span>
            </div>
          )}
          {approveField && (
            <div className="flex items-center gap-2 pt-2 border-t border-border-subtle">
              <Button
                variant="primary"
                size="sm"
                disabled={busy}
                onClick={() => submitWithVerdict(true)}
              >
                {busy ? "…" : "Approve"}
              </Button>
              <Button
                variant="danger"
                size="sm"
                disabled={busy}
                onClick={() => submitWithVerdict(false)}
              >
                {busy ? "…" : "Reject"}
              </Button>
            </div>
          )}
          {actionField && (
            <div className="flex flex-wrap items-center gap-2 pt-2 border-t border-border-subtle">
              {(actionField.enum_values ?? []).map((value, i) => (
                <Button
                  key={value}
                  variant={i === 0 ? "primary" : "secondary"}
                  size="sm"
                  disabled={busy}
                  onClick={() => submitWithVerdict(value)}
                >
                  {busy ? "…" : humanizeActionValue(value)}
                </Button>
              ))}
            </div>
          )}
          {showQuickActions && (
            <div className="flex items-center gap-2 pt-1">
              <span className="text-caption text-fg-subtle">Quick reply</span>
              {quickActions.map((qa) => (
                <button
                  key={qa}
                  type="button"
                  disabled={busy}
                  onClick={() => submitQuickAction(qa)}
                  className="px-2 py-0.5 rounded-full border border-border-subtle text-micro text-fg-muted hover:text-fg-default hover:border-border-strong disabled:opacity-50"
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

function makeHumanIdentityRequired(spec: FormSpec): FormSpec {
  return {
    ...spec,
    questions: spec.questions.map((question): FormQuestion => {
      if (
        (question.id === "reviewer" || question.id === "reviewer_id") &&
        question.kind === "free_text"
      ) {
        return {
          ...question,
          label: question.id === "reviewer" ? "Your name" : "Reviewer ID",
          required: true,
          rows: 1,
          placeholder: "Required",
        };
      }
      return question;
    }),
  };
}

function defaultAnswerForSpec(spec: FormSpec | null): FormAnswer {
  if (!spec) return {};
  return Object.fromEntries(
    spec.questions.map((question) => [
      question.id,
      question.kind === "checkbox"
        ? [...(question.defaultValues ?? [])]
        : "defaultValue" in question
          ? question.defaultValue ?? ""
          : "",
    ]),
  );
}

// humanizeActionValue turns an `action` enum token into a button label:
// "request_changes" → "Request changes".
function humanizeActionValue(value: string): string {
  const words = value.replace(/[_-]+/g, " ").trim();
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : value;
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
