import { useCallback, useEffect, useMemo, useState } from "react";
import {
  CheckCircledIcon,
  ExclamationTriangleIcon,
} from "@radix-ui/react-icons";
import { Link } from "wouter";
import { useQueryClient } from "@tanstack/react-query";

import { Button } from "@/components/ui/Button";
import { useAssistantActions } from "@/hooks/useAssistantActions";
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

type ExecutionState =
  | { status: "idle" }
  | { status: "running" }
  | { status: "success"; result: AssistantActionResult }
  | { status: "error"; message: string };

const SESSION_KEY_PREFIX = "iterion.assistant.executedAction.v1:";

function readExecution(key: string): ExecutionState {
  try {
    const raw = sessionStorage.getItem(SESSION_KEY_PREFIX + key);
    if (!raw) return { status: "idle" };
    const parsed = JSON.parse(raw) as ExecutionState;
    if (parsed.status === "success" || parsed.status === "error") return parsed;
    // A reload during a request must not fire the same write a second time.
    if (parsed.status === "running") {
      return {
        status: "error",
        message:
          "Execution was interrupted while its result was unknown. Check the target before retrying.",
      };
    }
  } catch {
    // Corrupt browser state grants nothing; return to an explicit action.
  }
  return { status: "idle" };
}

function writeExecution(key: string, state: ExecutionState): void {
  try {
    sessionStorage.setItem(SESSION_KEY_PREFIX + key, JSON.stringify(state));
  } catch {
    // The in-memory state and host/API validation remain authoritative.
  }
}

export default function AssistantActionOffer({
  runId,
  revision,
}: {
  runId: string | null;
  revision: number;
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
        />
      ))}
    </div>
  );
}

function ActionRequestCard({
  request,
  assistantRunId,
}: {
  request: AssistantActionRequest;
  assistantRunId: string | null;
}) {
  const queryClient = useQueryClient();
  const policy = useAssistantActionPolicy(request.id);
  const decision = decideAssistantAction(
    policy,
    request.intent === "explicit",
  );
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
    readExecution(request.key),
  );

  const execute = useCallback(async () => {
    if (!validated || decision === "deny" || execution.status === "running") {
      return;
    }
    const running: ExecutionState = { status: "running" };
    setExecution(running);
    writeExecution(request.key, running);
    try {
      const result = await executeAssistantAction(validated, {
        assistantRunId,
      });
      const success: ExecutionState = { status: "success", result };
      setExecution(success);
      writeExecution(request.key, success);
      // Mutations cross several screens (board → pipelines → run list). Let
      // active read models refresh from the server instead of guessing which
      // denormalized projections an action touched.
      void queryClient.invalidateQueries();
    } catch (error) {
      const failed: ExecutionState = {
        status: "error",
        message: error instanceof Error ? error.message : "Action failed",
      };
      setExecution(failed);
      writeExecution(request.key, failed);
    }
  }, [assistantRunId, decision, execution.status, queryClient, request.key, validated]);

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
      sessionStorage.removeItem(SESSION_KEY_PREFIX + request.key);
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
            {execution.status === "error" && validated && decision !== "deny" && (
              <Button variant="secondary" size="sm" onClick={retry}>
                Retry
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
          </div>
        </div>
      </div>
    </div>
  );
}
