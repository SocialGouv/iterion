import { useCallback, useEffect, useMemo, useState } from "react";
import {
  CheckCircledIcon,
  ExclamationTriangleIcon,
} from "@radix-ui/react-icons";
import { Link } from "wouter";
import { useQueryClient } from "@tanstack/react-query";

import { Button } from "@/components/ui/Button";
import { useAssistantActions } from "@/hooks/useAssistantActions";
import * as runsApi from "@/api/runs";
import MissionGrantControl from "@/components/ChatDock/MissionGrantControl";
import { missionGrantCovers } from "@/lib/chatDock/missionGrant";
import {
  decideAssistantAction,
  useAssistantActionPolicy,
  type AssistantActionRequest,
} from "@/lib/chatDock/assistantActions";
import {
  executeAssistantAction,
  validateAssistantActionRequest,
  type AssistantActionResult,
} from "@/lib/chatDock/assistantActionRequests";
import {
  isEditorSessionActive,
  resolveAuthoringSnapshot,
  resolveEditorSession,
} from "@/lib/chatDock/editorSession";
import { isResumeSourceChangedError } from "@/lib/runResume";

type ExecutionState =
  | { status: "idle" }
  | { status: "running" }
  | { status: "source-drift"; message: string }
  | { status: "resume-unavailable"; message: string }
  | { status: "launch-unavailable"; message: string }
  | { status: "success"; result: AssistantActionResult }
  | { status: "error"; message: string };

const HOST_FAILURE_MESSAGE_MAX = 2_000;
const HOST_FAILURE_VALUE_MAX = 512;
const HOST_FAILURE_FIELD_MAX = 32;
const FULL_GIT_COMMIT_SHA = /^[0-9a-f]{40}$/;

function isSensitiveHostField(name: string): boolean {
  return /(?:api[_-]?key|authorization|credential|password|secret|token)/i.test(name);
}

// A failed action must wake Copi so it can choose a safe alternative, but the
// host-event transcript is not a place to echo arbitrary action data. Keep
// the bounded, validated shape useful for diagnosis while redacting common
// credential fields and truncating unbounded transport error text.
function safeHostFailureValue(value: unknown, depth = 0): unknown {
  if (typeof value === "string") return value.slice(0, HOST_FAILURE_VALUE_MAX);
  if (typeof value === "number" || typeof value === "boolean" || value === null) return value;
  if (depth >= 3 || Array.isArray(value) && value.length > HOST_FAILURE_FIELD_MAX) {
    return "[omitted]";
  }
  if (Array.isArray(value)) return value.map((entry) => safeHostFailureValue(entry, depth + 1));
  if (typeof value !== "object") return String(value).slice(0, HOST_FAILURE_VALUE_MAX);
  const out: Record<string, unknown> = {};
  for (const [key, entry] of Object.entries(value as Record<string, unknown>).slice(0, HOST_FAILURE_FIELD_MAX)) {
    out[key] = isSensitiveHostField(key) ? "[redacted]" : safeHostFailureValue(entry, depth + 1);
  }
  return out;
}

function safeHostFailureMessage(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error ?? "Action failed");
  return message
    .replace(/((?:api[_-]?key|authorization|credential|password|secret|token)\s*[:=]\s*)\S+/gi, "$1[redacted]")
    .slice(0, HOST_FAILURE_MESSAGE_MAX);
}

// Completion receipts are data from host action executors, not a model output.
// Rebuild the one permitted shape here so a future executor cannot relay an
// arbitrary response into Copi's host-event transcript.
function safeHostCompletionReceipt(value: AssistantActionResult["receipt"]):
  | { git_commit: string }
  | { created_bot: { name: string; editor_path: string } }
  | undefined {
  if (!value) return undefined;
  if (
    "git_commit" in value &&
    typeof value.git_commit === "string" &&
    FULL_GIT_COMMIT_SHA.test(value.git_commit)
  ) return { git_commit: value.git_commit };
  if (
    "created_bot" in value &&
    typeof value.created_bot?.name === "string" &&
    /^[a-z0-9][a-z0-9_-]*$/.test(value.created_bot.name) &&
    typeof value.created_bot.editor_path === "string" &&
    new RegExp(`^bots/${value.created_bot.name}/main\\.bot$`).test(
      value.created_bot.editor_path,
    )
  ) {
    return {
      created_bot: {
        name: value.created_bot.name,
        editor_path: value.created_bot.editor_path,
      },
    };
  }
  return undefined;
}

// Action requests live in the persisted assistant transcript, so their
// execution receipts must outlive a browser process too. sessionStorage made
// an already-executed card actionable again after a computer/browser restart;
// replaying an old resume against a run that had since advanced then produced
// a confusing status conflict (and less idempotent actions could be worse).
const ACTION_KEY_PREFIX = "iterion.assistant.executedAction.v2:";
const LEGACY_SESSION_KEY_PREFIX = "iterion.assistant.executedAction.v1:";

function resumeUnavailableMessage(value: unknown): string | null {
  const message = value instanceof Error ? value.message : String(value ?? "");
  const match = message.match(/cannot be resumed \(status: ([^)]+)\)/i);
  if (!match) return null;
  const status = match[1]?.trim() || "no longer resumable";
  if (status === "failed") {
    return "This run has since reached failed. Open it and rewind to an earlier node before resuming; retrying the same resume cannot succeed.";
  }
  return `This run is now ${status} and can no longer be resumed from this action. Open the run to inspect its current state.`;
}

function delegatedLaunchUnavailableMessage(value: unknown): string | null {
  const message = value instanceof Error ? value.message : String(value ?? "");
  if (
    !/delegated (?:launch|worker)/i.test(message) ||
    !/(?:worktree: auto|not a repair worker|must declare var)/i.test(message)
  ) {
    return null;
  }
  return "This catalog bot cannot repair a failed source run. Delegation requires an isolated repair worker with worktree: auto and the host-owned delegation variables; retrying the same launch cannot succeed.";
}

function resolveAuthoringCommitSnapshot(
  request: AssistantActionRequest,
) {
  if (
    request.id !== "authoring.git.commit" &&
    request.id !== "authoring.git.publish" &&
    request.id !== "dependency.bots.localize"
  ) return undefined;
  const sessionId = request.args.editor_session_id;
  const revision = request.args.editor_revision;
  if (
    typeof sessionId !== "string" ||
    typeof revision !== "number" ||
    !Number.isInteger(revision)
  ) {
    throw new Error("The authoring commit is missing its host-attested editor session.");
  }
  const session = resolveEditorSession(sessionId);
  if (
    !session ||
    !isEditorSessionActive(sessionId) ||
    session.store.getState()._generation !== revision ||
    session.store.getState().isDirty()
  ) {
    throw new Error("Return to the unchanged editor tab before committing declared authoring files.");
  }
  const snapshot = resolveAuthoringSnapshot(sessionId, revision);
  if (!snapshot) {
    throw new Error("The original authoring snapshot is unavailable. Ask Copi again from the open bot.");
  }
  return snapshot;
}

function readExecution(
  key: string,
  actionId: AssistantActionRequest["id"],
): ExecutionState {
  try {
    const durableKey = ACTION_KEY_PREFIX + key;
    const raw =
      localStorage.getItem(durableKey) ??
      sessionStorage.getItem(LEGACY_SESSION_KEY_PREFIX + key);
    if (!raw) return { status: "idle" };
    const parsed = JSON.parse(raw) as ExecutionState;
    // Migrate cards that failed before the dedicated source-drift state
    // existed. The error text was already persisted in sessionStorage; after
    // a deploy/refresh it should gain the safe force affordance immediately,
    // not send the operator through the same doomed Retry again.
    let state: ExecutionState;
    if (
      parsed.status === "error" &&
      actionId === "run.resume" &&
      isResumeSourceChangedError(parsed.message)
    ) {
      state = {
        status: "source-drift",
        message:
          "This run was created from an older workflow source. Resuming with the current source may change what happens after the saved checkpoint.",
      };
    } else if (
      parsed.status === "error" &&
      actionId === "run.resume" &&
      resumeUnavailableMessage(parsed.message)
    ) {
      state = {
        status: "resume-unavailable",
        message: resumeUnavailableMessage(parsed.message) as string,
      };
    } else if (
      parsed.status === "error" &&
      actionId === "run.launch" &&
      delegatedLaunchUnavailableMessage(parsed.message)
    ) {
      state = {
        status: "launch-unavailable",
        message: delegatedLaunchUnavailableMessage(parsed.message) as string,
      };
    } else if (
      parsed.status === "success" ||
      parsed.status === "error" ||
      parsed.status === "source-drift" ||
      parsed.status === "resume-unavailable" ||
      parsed.status === "launch-unavailable"
    ) {
      state = parsed;
    } else if (
      parsed.status === "running" &&
      actionId === "workspace.handoff.complete"
    ) {
      // The server owns idempotency for terminal handoff receipts. A reload
      // after it accepted the POST but before Studio stored success is safe to
      // retry and is required to avoid stranding the source conversation.
      state = { status: "idle" };
    } else if (parsed.status === "running") {
      // A reload during a request must not fire the same write a second time.
      state = {
        status: "error",
        message:
          "Execution was interrupted while its result was unknown. Check the target before retrying.",
      };
    } else {
      state = { status: "idle" };
    }
    // This also migrates a v1 session receipt and any old generic error state.
    localStorage.setItem(durableKey, JSON.stringify(state));
    return state;
  } catch {
    // Corrupt browser state grants nothing; return to an explicit action.
  }
  return { status: "idle" };
}

function writeExecution(key: string, state: ExecutionState): void {
  try {
    localStorage.setItem(ACTION_KEY_PREFIX + key, JSON.stringify(state));
  } catch {
    // The in-memory state and host/API validation remain authoritative.
  }
}

export default function AssistantActionOffer({
  runId,
  revision,
  workspaceHandoffId = null,
}: {
  runId: string | null;
  revision: number;
  workspaceHandoffId?: string | null;
}) {
  const requests = useAssistantActions(runId, revision);
  if (requests.length === 0) return null;
  return (
    <div className="mt-3 space-y-2">
      {requests.map((request) => (
        <ActionRequestCard
          key={request.key}
          request={request}
          assistantRunId={runId}
          workspaceHandoffId={workspaceHandoffId}
        />
      ))}
    </div>
  );
}

function ActionRequestCard({
  request,
  assistantRunId,
  workspaceHandoffId,
}: {
  request: AssistantActionRequest;
  assistantRunId: string | null;
  workspaceHandoffId: string | null;
}) {
  const queryClient = useQueryClient();
  const policy = useAssistantActionPolicy(request.id);
  // A mission grant is the operator saying "see THIS run through", bounded to
  // one assistant, one target, a short verb list and an expiry. It overrides
  // the ordinary policy only inside those bounds; everything else still asks.
  const granted = missionGrantCovers(
    assistantRunId,
    typeof request.args?.run_id === "string" ? request.args.run_id : null,
    request.id,
  );
  const decision = granted
    ? "auto"
    : decideAssistantAction(policy, request.intent === "explicit");
  const validation = useMemo(() => {
    try {
      return { value: validateAssistantActionRequest(request), error: null };
    } catch (error) {
      return {
        value: null,
        error:
          error instanceof Error ? error.message : "Invalid action request",
      };
    }
  }, [request]);
  const validated = validation.value;
  const [execution, setExecution] = useState<ExecutionState>(() =>
    readExecution(request.key, request.id),
  );

  const execute = useCallback(async (forceResume = false) => {
    if (!validated || decision === "deny" || execution.status === "running") {
      return;
    }
    const running: ExecutionState = { status: "running" };
    setExecution(running);
    writeExecution(request.key, running);
    try {
      const authoringSnapshot = resolveAuthoringCommitSnapshot(request);
      const result = await executeAssistantAction(validated, {
        assistantRunId,
        forceResume,
        ...(workspaceHandoffId ? { workspaceHandoffId } : {}),
        ...(authoringSnapshot ? { authoringSnapshot } : {}),
      });
      const success: ExecutionState = { status: "success", result };
      setExecution(success);
      writeExecution(request.key, success);
      // Hand the turn BACK to the assistant. Without this it proposed an
      // action, the action ran, and it was never told — so a repair needing
      // five steps got woken at one of them and waited for the operator to
      // prompt the rest. Best-effort and deliberately unawaited for the UI: a
      // 409 only means the assistant is mid-turn, and losing the signal must
      // never turn an action that SUCCEEDED into a failed one on screen.
      if (assistantRunId) {
        const receipt = safeHostCompletionReceipt(result.receipt);
        void runsApi
          .deliverHostEvent(assistantRunId, "action-completed", {
            action: validated.id,
            args: validated.args,
            message: result.message,
            ...(receipt ? { receipt } : {}),
          })
          .catch(() => undefined);
      }
      // Mutations cross several screens (board → pipelines → run list). Let
      // active read models refresh from the server instead of guessing which
      // denormalized projections an action touched.
      void queryClient.invalidateQueries();
    } catch (error) {
      const hostFailureMessage = safeHostFailureMessage(error);
      // Source drift on the normal resume is an intermediate consent state,
      // not a terminal action failure. Waking Copi here would unpark its chat
      // turn and replace this offer before the operator could choose the
      // host-only force path. A forced attempt that still fails deliberately
      // falls through and is reported like every other final failure.
      //
      // Use the structured live classifier here. The prose-only helper above
      // remains for migrating already-persisted legacy error messages.
      if (
        !forceResume &&
        validated.id === "run.resume" &&
        runsApi.isWorkflowSourceChangedError(error)
      ) {
        const drift: ExecutionState = {
          status: "source-drift",
          message:
            "This run was created from an older workflow source. Resuming with the current source may change what happens after the saved checkpoint.",
        };
        setExecution(drift);
        writeExecution(request.key, drift);
        return;
      }
      if (assistantRunId) {
        void runsApi
          .deliverHostEvent(assistantRunId, "action-failed", {
            action: validated.id,
            args: safeHostFailureValue(validated.args),
            message: hostFailureMessage,
          })
          .catch(() => undefined);
      }
      if (validated.id === "run.resume") {
        const message = resumeUnavailableMessage(error);
        if (message) {
          const unavailable: ExecutionState = {
            status: "resume-unavailable",
            message,
          };
          setExecution(unavailable);
          writeExecution(request.key, unavailable);
          return;
        }
      }
      if (validated.id === "run.launch") {
        const message = delegatedLaunchUnavailableMessage(error);
        if (message) {
          const unavailable: ExecutionState = {
            status: "launch-unavailable",
            message,
          };
          setExecution(unavailable);
          writeExecution(request.key, unavailable);
          return;
        }
      }
      const failed: ExecutionState = {
        status: "error",
        message: error instanceof Error ? error.message : "Action failed",
      };
      setExecution(failed);
      writeExecution(request.key, failed);
    }
  }, [assistantRunId, decision, execution.status, queryClient, request.key, validated, workspaceHandoffId]);

  useEffect(() => {
    if (decision !== "auto" || execution.status !== "idle" || !validated) {
      return;
    }
    // Defer the mutation out of the effect body. Besides avoiding a cascading
    // synchronous render, this gives React a chance to commit the visible
    // offer before an automatically authorised request starts.
    const timer = window.setTimeout(() => void execute(), 0);
    return () => window.clearTimeout(timer);
  }, [decision, execute, execution.status, validated]);

  const retry = () => {
    const idle: ExecutionState = { status: "idle" };
    setExecution(idle);
    try {
      localStorage.removeItem(ACTION_KEY_PREFIX + request.key);
      sessionStorage.removeItem(LEGACY_SESSION_KEY_PREFIX + request.key);
    } catch {
      // A manual retry still works in memory.
    }
  };

  return (
    <div className="rounded-md border border-border-subtle bg-surface-2 p-2.5">
      <div className="flex items-start gap-2">
        {execution.status === "success" ? (
          <CheckCircledIcon
            className="mt-0.5 h-4 w-4 shrink-0 text-success-fg"
            aria-hidden="true"
          />
        ) : (
          <ExclamationTriangleIcon
            className="mt-0.5 h-4 w-4 shrink-0 text-accent-text"
            aria-hidden="true"
          />
        )}
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <p className="text-label font-medium">
              {validated?.title ?? "Invalid assistant action"}
            </p>
            {validated && (
              <span className="rounded-full border border-border-subtle px-1.5 py-0.5 text-micro uppercase tracking-wide text-fg-subtle">
                {validated.definition.risk}
              </span>
            )}
          </div>
          <p className="mt-0.5 text-caption text-fg-muted">
            {validation.error ?? validated?.detail}
          </p>
          {decision === "deny" && validated && (
            <p className="mt-1 text-caption text-danger-fg">
              Blocked by Settings → Assistant.
            </p>
          )}
          {execution.status === "running" && (
            <p className="mt-1 text-caption text-fg-muted">Executing…</p>
          )}
          {execution.status === "success" && (
            <p className="mt-1 text-caption text-success-fg">
              {execution.result.message}
            </p>
          )}
          {execution.status === "error" && (
            <p className="mt-1 text-caption text-danger-fg">
              {execution.message}
            </p>
          )}
          {execution.status === "source-drift" && (
            <p className="mt-1 text-caption text-accent-text">
              {execution.message}
            </p>
          )}
          {execution.status === "resume-unavailable" && (
            <p className="mt-1 text-caption text-danger-fg">
              {execution.message}
            </p>
          )}
          {execution.status === "launch-unavailable" && (
            <p className="mt-1 text-caption text-danger-fg">
              {execution.message}
            </p>
          )}
          <div className="mt-2 flex flex-wrap gap-2">
            {validated &&
              decision === "confirm" &&
              execution.status === "idle" && (
                <Button
                  variant={
                    validated.definition.risk === "destructive"
                      ? "danger"
                      : "primary"
                  }
                  size="sm"
                  onClick={() => void execute()}
                >
                  Confirm action
                </Button>
              )}
            {/* Offered NEXT TO Confirm, never instead of it: an operator who
                wants to approve every step keeps doing so. This is only for
                the case the per-action policy could not express — delegating
                one job on one run, with an expiry. */}
            {validated &&
              validated.id !== "workspace.handoff.complete" &&
              execution.status === "idle" && (
              <MissionGrantControl
                assistantRunId={assistantRunId}
                targetRunId={
                  typeof request.args?.run_id === "string"
                    ? request.args.run_id
                    : null
                }
                action={request.id}
                assistantLabel="Copi"
              />
            )}
            {execution.status === "error" && validated && decision !== "deny" && (
              <Button variant="secondary" size="sm" onClick={retry}>
                Retry
              </Button>
            )}
            {execution.status === "source-drift" &&
              validated &&
              decision !== "deny" && (
                <Button
                  variant="primary"
                  size="sm"
                  onClick={() => void execute(true)}
                >
                  Resume with updated workflow
                </Button>
              )}
            {execution.status === "success" && execution.result.href && (
              <Link
                href={execution.result.href}
                className="inline-flex h-7 items-center justify-center rounded-md border border-border-default bg-surface-2 px-2.5 text-xs font-medium text-fg-default hover:bg-surface-3"
              >
                {execution.result.hrefLabel ?? "Open"}
              </Link>
            )}
            {execution.status === "resume-unavailable" && validated && (
              <Link
                href={`/runs/${encodeURIComponent(String(validated.args.run_id))}`}
                className="inline-flex h-7 items-center justify-center rounded-md border border-border-default bg-surface-2 px-2.5 text-xs font-medium text-fg-default hover:bg-surface-3"
              >
                Open run
              </Link>
            )}
            {execution.status === "launch-unavailable" &&
              validated &&
              typeof validated.args.source_run_id === "string" && (
                <Link
                  href={`/runs/${encodeURIComponent(validated.args.source_run_id)}`}
                  className="inline-flex h-7 items-center justify-center rounded-md border border-border-default bg-surface-2 px-2.5 text-xs font-medium text-fg-default hover:bg-surface-3"
                >
                  Open failed run
                </Link>
              )}
          </div>
        </div>
      </div>
    </div>
  );
}
