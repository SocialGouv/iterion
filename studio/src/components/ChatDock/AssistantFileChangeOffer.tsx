import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { CheckIcon, ExclamationTriangleIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import {
  commitAssistantAuthoring,
  previewAssistantAuthoring,
  snapshotAssistantAuthoring,
  type AssistantAuthoringSnapshot,
  type AssistantAuthoringPreviewFile,
  type AssistantAuthoringValidation,
} from "@/api/assistantAuthoring";
import * as api from "@/api/client";
import * as runsApi from "@/api/runs";
import type { AssistantFileChange } from "@/api/runs/artifacts";
import { Button } from "@/components/ui/Button";
import { useFileChangeProposal } from "@/hooks/useFileChangeProposal";
import {
  decideAssistantAction,
  useAssistantActionPolicy,
} from "@/lib/chatDock/assistantActions";
import {
  isEditorSessionActive,
  resolveAuthoringSnapshot,
  resolveEditorSession,
} from "@/lib/chatDock/editorSession";
import { useUIStore } from "@/store/ui";

import AssistantTextDiffDialog from "./AssistantTextDiffDialog";

type State = "idle" | "previewing" | "ready" | "saving" | "saved" | "error";

type AuthoringFailurePhase = "preview" | "save";

const authoringFailureMessageMaxChars = 500;
const authoringFailureFileMaxCount = 8;
const authoringFailureDeliveryMaxAttempts = 2;
const authoringFailureRegistryMaxEntries = 512;

type AuthoringFailureDeliveryState = {
  delivered: number;
  inFlight: Set<string>;
};

// This registry intentionally outlives an offer component. ChatDock unmounts
// the offer while Copi handles a host event, so component state would reset on
// every corrective proposal and could not bound an allow-policy retry loop.
const authoringFailureDeliveries = new Map<string, AuthoringFailureDeliveryState>();

function authoringFailureKey(runId: string, sessionId: string, editorPath: string): string {
  return `${runId}:${sessionId}:${editorPath}`;
}

function authoringFailureState(key: string): AuthoringFailureDeliveryState {
  const existing = authoringFailureDeliveries.get(key);
  if (existing) return existing;
  if (authoringFailureDeliveries.size >= authoringFailureRegistryMaxEntries) {
    const oldest = authoringFailureDeliveries.keys().next().value;
    if (oldest) authoringFailureDeliveries.delete(oldest);
  }
  const state = { delivered: 0, inFlight: new Set<string>() };
  authoringFailureDeliveries.set(key, state);
  return state;
}

function clearAuthoringFailureState(runId: string | null, sessionId: string | null, editorPath: string | undefined) {
  if (!runId || !sessionId || !editorPath) return;
  authoringFailureDeliveries.delete(authoringFailureKey(runId, sessionId, editorPath));
}

function boundedAuthoringFailureDetail(error: unknown, phase: AuthoringFailurePhase): string {
  const raw = error instanceof Error ? error.message : String(error ?? "");
  // Server validation names structural positions, never replacement bytes.
  // Preserve only that known-safe detail; API clients may otherwise include a
  // raw response body, which must not become assistant context.
  const structural = raw.match(/changes\[\d+\]\.replacements\[\d+\]: before text matched \d+ times, want exactly once/i);
  if (structural) return structural[0].slice(0, authoringFailureMessageMaxChars);
  const status = error instanceof api.ApiError ? ` (HTTP ${error.status})` : "";
  return `Studio rejected the authoring ${phase}${status}; re-read the current files before proposing a correction.`
    .slice(0, authoringFailureMessageMaxChars);
}

function authoringFailureReceipt(
  snapshot: AssistantAuthoringSnapshot,
  sessionId: string,
  revision: number,
  phase: AuthoringFailurePhase,
  attempt: number,
  error: unknown,
) {
  return {
    action: "editor.files.save",
    phase,
    attempt,
    message: boundedAuthoringFailureDetail(error, phase),
    args: {
      editor_session_id: sessionId,
      editor_revision: revision,
      editor_path: snapshot.editor_path,
      files: snapshot.files.slice(0, authoringFailureFileMaxCount).map((file) => ({
        scope: file.scope,
        path: file.path,
        available: file.available,
        readable: file.readable,
      })),
    },
  };
}

function waitForFailureDelivery(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}

type FailureDeliveryResult = "delivered" | "in-flight" | "capped" | "undelivered";

async function deliverAuthoringFailure(
  runId: string,
  snapshot: AssistantAuthoringSnapshot,
  sessionId: string,
  revision: number,
  phase: AuthoringFailurePhase,
  error: unknown,
): Promise<FailureDeliveryResult> {
  const key = authoringFailureKey(runId, sessionId, snapshot.editor_path);
  const state = authoringFailureState(key);
  if (state.delivered >= authoringFailureDeliveryMaxAttempts) return "capped";
  const inFlightKey = `${phase}:${revision}:${boundedAuthoringFailureDetail(error, phase)}`;
  if (state.inFlight.has(inFlightKey)) return "in-flight";
  state.inFlight.add(inFlightKey);
  try {
    const attempt = state.delivered + 1;
    const receipt = authoringFailureReceipt(snapshot, sessionId, revision, phase, attempt, error);
    for (const delay of [0, 400, 1_000]) {
      if (delay > 0) await waitForFailureDelivery(delay);
      try {
        await runsApi.deliverHostEvent(runId, "action-failed", receipt);
        state.delivered++;
        return "delivered";
      } catch (deliveryError) {
        if (!(deliveryError instanceof api.ApiError) || deliveryError.status !== 409) {
          return "undelivered";
        }
      }
    }
    return "undelivered";
  } finally {
    state.inFlight.delete(inFlightKey);
  }
}

export function isActiveAuthoringTarget(
  snapshot: AssistantAuthoringSnapshot | null,
  changes: readonly AssistantFileChange[],
): boolean {
  const active = snapshot?.active_file;
  return !!active && changes.some((change) =>
    change.scope === active.scope && change.path === active.path,
  );
}

function authoringSaveReceipt(
  sessionId: string,
  revision: number,
  files: readonly AssistantAuthoringPreviewFile[],
  validation?: AssistantAuthoringValidation,
  authoring?: AssistantAuthoringSnapshot,
) {
  const args = {
    editor_session_id: sessionId,
    editor_revision: revision,
    // A completion receipt is a small host fact, not a second copy of the
    // model's source. The paths and operation tell Copi what it may verify
    // next without exposing source, hashes, or arbitrary commit response data.
    files: files.map((file) => ({
      scope: file.scope,
      path: file.path,
      operation: file.operation ?? "replace",
    })),
    ...(validation ? { validation } : {}),
    ...(authoring ? { authoring } : {}),
  };
  return {
    action: "editor.files.save",
    args,
    message: `Saved ${files.length} authoring file change${files.length === 1 ? "" : "s"}`,
  };
}

export default function AssistantFileChangeOffer({
  runId,
  revision,
}: {
  runId: string | null;
  revision: number;
}) {
  const proposal = useFileChangeProposal(runId, revision);
  const [route] = useLocation();
  const addToast = useUIStore((state) => state.addToast);
  const policy = useAssistantActionPolicy("editor.files.save");
  const decision = decideAssistantAction(policy, proposal.intent === "explicit");
  const [state, setState] = useState<State>("idle");
  const [error, setError] = useState<string | null>(null);
  const [failureCapReached, setFailureCapReached] = useState(false);
  const [reloadWarning, setReloadWarning] = useState<string | null>(null);
  const [preview, setPreview] = useState<AssistantAuthoringPreviewFile[]>([]);
  const [selected, setSelected] = useState<AssistantAuthoringPreviewFile | null>(null);
  const autoStarted = useRef<string | null>(null);

  const session = proposal.sessionId
    ? resolveEditorSession(proposal.sessionId)
    : null;
  const snapshot =
    proposal.sessionId && proposal.revision !== null
      ? resolveAuthoringSnapshot(proposal.sessionId, proposal.revision)
      : null;
  const subscribeRevision = useCallback(
    (notify: () => void) => session?.store.subscribe(notify) ?? (() => {}),
    [session?.store],
  );
  const readRevision = useCallback(
    () => session?.store.getState()._generation ?? null,
    [session?.store],
  );
  const currentRevision = useSyncExternalStore(
    subscribeRevision,
    readRevision,
    () => null,
  );
  const unavailable =
    !proposal.sessionId ||
    proposal.revision === null ||
    !route.startsWith("/editor") ||
    !isEditorSessionActive(proposal.sessionId) ||
    currentRevision !== proposal.revision ||
    !snapshot;
  const activeTarget = isActiveAuthoringTarget(snapshot, proposal.changes);
  const activeTargetUnsafe = activeTarget && (
    !session ||
    currentRevision !== proposal.revision ||
    session.store.getState().isDirty()
  );
  const blocked = unavailable || activeTargetUnsafe;
  const reloadRef = useRef<() => void>(() => {});

  const warnReload = useCallback((message: string) => {
    setReloadWarning(message);
    addToast(message, "warning", {
      persistent: true,
      action: {
        label: "Retry reload",
        onClick: () => reloadRef.current(),
      },
    });
  }, [addToast]);

  const reloadActiveFile = useCallback(async (): Promise<boolean> => {
    if (!activeTarget || !proposal.sessionId || proposal.revision === null) return true;
    const current = resolveEditorSession(proposal.sessionId);
    const before = current?.store.getState();
    const path = before?.currentFilePath;
    if (
      !current ||
      !path ||
      !isEditorSessionActive(proposal.sessionId) ||
      before._generation !== proposal.revision ||
      before.isDirty()
    ) {
      warnReload("Authoring file changes were saved, but the open tab changed and was not reloaded.");
      return false;
    }
    try {
      const result = await api.openFile(path);
      const after = resolveEditorSession(proposal.sessionId);
      const store = after?.store.getState();
      if (
        !after ||
        !isEditorSessionActive(proposal.sessionId) ||
        !store ||
        store.currentFilePath !== path ||
        store._generation !== proposal.revision ||
        store.isDirty()
      ) {
        warnReload("Authoring file changes were saved, but the open tab changed and was not reloaded.");
        return false;
      }
      store.setDocument(result.document);
      store.setDiagnostics(result.diagnostics);
      store.setCurrentSource(result.source);
      store.markSaved();
      setReloadWarning(null);
      return true;
    } catch {
      warnReload("Authoring file changes were saved, but the open tab could not be reloaded.");
      return false;
    }
  }, [activeTarget, proposal.sessionId, proposal.revision, warnReload]);
  reloadRef.current = () => { void reloadActiveFile(); };

  const reportFailureToCopi = useCallback((phase: AuthoringFailurePhase, failure: unknown) => {
    if (!runId || !proposal.sessionId || proposal.revision === null || !snapshot) return;
    void deliverAuthoringFailure(runId, snapshot, proposal.sessionId, proposal.revision, phase, failure)
      .then((result) => {
        if (result === "capped") setFailureCapReached(true);
      });
  }, [runId, snapshot, proposal.sessionId, proposal.revision]);

  const review = useCallback(async () => {
    if (!snapshot || proposal.changes.length === 0 || blocked) {
      setError(activeTargetUnsafe
        ? "Save or discard the active editor buffer before reviewing a change to that file."
        : "The editor tab or revision changed. Ask Copi again from the open bot.");
      setState("error");
      return;
    }
    setState("previewing");
    setError(null);
    try {
      const result = await previewAssistantAuthoring(snapshot, proposal.changes);
      setPreview(result.files);
      setSelected(result.files[0] ?? null);
      setState("ready");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not preview the changes");
      setState("error");
      reportFailureToCopi("preview", err);
    }
  }, [snapshot, proposal.changes, blocked, activeTargetUnsafe, reportFailureToCopi]);

  const save = useCallback(async () => {
    if (!snapshot || proposal.changes.length === 0 || blocked) {
      if (activeTargetUnsafe) {
        setError("Save or discard the active editor buffer before saving a change to that file.");
        setState("error");
      }
      return;
    }
    setState("saving");
    setError(null);
    setReloadWarning(null);
    try {
      const result = await commitAssistantAuthoring(snapshot, proposal.changes);
      clearAuthoringFailureState(runId, proposal.sessionId, snapshot.editor_path);
      setPreview(result.files);
      setState("saved");
      if (activeTarget) {
        if (await reloadActiveFile()) {
          addToast(
            `${result.files.length} authoring file change${result.files.length === 1 ? "" : "s"} saved and reloaded`,
            "success",
          );
        }
      } else {
        addToast(
          `${result.files.length} authoring file change${result.files.length === 1 ? "" : "s"} saved`,
          "success",
        );
      }
      // A saved proposal is a completed host action. Copi's prompt waits for
      // this receipt before it runs verification or requests recovery; without
      // it the conversation stays parked and the operator has to send a
      // meaningless follow-up. Keep delivery best-effort, like typed actions:
      // a chat-boundary race must never turn a successful save into an error.
      if (runId && proposal.sessionId && proposal.revision !== null) {
        let freshAuthoring: AssistantAuthoringSnapshot | undefined;
        try {
          freshAuthoring = await snapshotAssistantAuthoring(snapshot.editor_path);
        } catch {
          // The save remains successful. Copi will request a fresh editor
          // attachment if this bounded metadata refresh is unavailable.
        }
        void runsApi
          .deliverHostEvent(
            runId,
            "action-completed",
            authoringSaveReceipt(
              proposal.sessionId,
              proposal.revision,
              result.files,
              result.validation,
              freshAuthoring,
            ),
          )
          .catch(() => undefined);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save the changes");
      setState("error");
      reportFailureToCopi("save", err);
    }
  }, [snapshot, proposal.changes, proposal.sessionId, proposal.revision, blocked, activeTargetUnsafe, activeTarget, reloadActiveFile, addToast, runId, reportFailureToCopi]);

  useEffect(() => {
    if (
      decision !== "auto" ||
      blocked ||
      state !== "idle" ||
      !proposal.sessionId ||
      proposal.revision === null
    ) {
      return;
    }
    const key = `${proposal.sessionId}:${proposal.revision}`;
    if (autoStarted.current === key) return;
    autoStarted.current = key;
    void save();
  }, [decision, blocked, state, proposal.sessionId, proposal.revision, save]);

  if (proposal.changes.length === 0 || !proposal.sessionId || proposal.revision === null) {
    return null;
  }

  let detail = `${proposal.changes.length} authoring file change${proposal.changes.length === 1 ? "" : "s"}. Declared creations and exact replacements are validated during preview; behavioral tests are not run in v1.`;
  if (state === "saved") detail = reloadWarning ?? "Files saved after fresh hash, compile, and syntax checks. Behavioral tests were not run.";
  else if (!snapshot) detail = "The authoring snapshot for this editor turn is no longer available. Ask Copi again from the open bot.";
  else if (activeTargetUnsafe) detail = "Save or discard the active editor buffer before changing that file.";
  else if (unavailable) detail = "Return to the unchanged editor tab before reviewing or saving these files.";
  else if (decision === "deny") detail = "Assistant authoring saves are disabled in Settings → Assistant.";
  else if (state === "previewing") detail = "Resolving exact replacements and compiling changed bot files…";
  else if (state === "ready") detail = "Preview ready. Review any file, then confirm the save.";
  else if (state === "saving") detail = "Rechecking hashes and saving the reviewed files…";
  else if (failureCapReached) detail = "Copi was not notified after two automatic recovery attempts. Reply in chat to continue.";

  return (
    <div className="mt-3 rounded-md border border-border-subtle bg-surface-2 p-2.5">
      <div className="flex items-start gap-2">
        {state === "saved" ? (
          <CheckIcon className="mt-0.5 h-4 w-4 text-success-fg" aria-hidden="true" />
        ) : (
          <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 text-accent-text" aria-hidden="true" />
        )}
        <div className="min-w-0 flex-1">
          <p className="text-label font-medium">
            {state === "saved" ? "Authoring file changes saved" : "Proposed authoring file changes"}
          </p>
          <p className="mt-0.5 text-caption text-fg-muted">{detail}</p>
          {error && <p className="mt-1 text-caption text-danger-fg">{error}</p>}
          {preview.length > 0 && (
            <div className="mt-2 flex flex-wrap gap-1.5">
              {preview.map((file) => (
                <Button
                  key={`${file.scope}:${file.path}`}
                  variant="secondary"
                  size="sm"
                  onClick={() => setSelected(file)}
                >
                  {file.operation === "create" ? `Create ${file.path}` : file.path}
                </Button>
              ))}
            </div>
          )}
          <div className="mt-2 flex flex-wrap gap-2">
            {state !== "saved" && decision !== "deny" && (
              <Button
                variant="secondary"
                size="sm"
                disabled={blocked || state === "previewing" || state === "saving"}
                onClick={() => void review()}
              >
                {state === "previewing" ? "Preparing preview…" : "Review changes"}
              </Button>
            )}
            {state === "ready" && decision === "confirm" && (
              <Button
                variant="primary"
                size="sm"
                disabled={blocked}
                onClick={() => void save()}
              >
                Save authoring changes
              </Button>
            )}
            {state === "saved" && reloadWarning && (
              <Button variant="secondary" size="sm" onClick={() => reloadRef.current()}>
                Retry reload
              </Button>
            )}
          </div>
        </div>
      </div>
      <AssistantTextDiffDialog file={selected} onClose={() => setSelected(null)} />
    </div>
  );
}
