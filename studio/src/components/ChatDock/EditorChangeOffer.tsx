// A validated assistant proposal becomes a Studio action, never a file tool.
// The operator's global action policy decides whether Iterion denies, asks,
// or executes. Every path still checks the exact live tab + revision; saving
// can only reuse the path already bound to that tab.

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { CheckIcon, ExclamationTriangleIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import * as api from "@/api/client";
import { Button } from "@/components/ui/Button";
import { useEditorProposal } from "@/hooks/useEditorProposal";
import {
  decideAssistantAction,
  useAssistantActionPolicy,
} from "@/lib/chatDock/assistantActions";
import {
  isEditorSessionActive,
  resolveEditorSession,
} from "@/lib/chatDock/editorSession";
import { useRecentsStore } from "@/store/recents";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";

type ActionState = "idle" | "applying" | "applied" | "saving" | "saved" | "error";

export default function EditorChangeOffer({
  runId,
  revision,
}: {
  runId: string | null;
  revision: number;
}) {
  const proposal = useEditorProposal(runId, revision);
  const [route, setLocation] = useLocation();
  const activeTabId = useTabsStore((state) => state.activeEditorTabId);
  const isCloud = useServerInfoStore((state) => state.info?.mode === "cloud");
  const addToast = useUIStore((state) => state.addToast);
  const pushRecent = useRecentsStore((state) => state.pushRecent);
  const applyPolicy = useAssistantActionPolicy("editor.apply");
  const savePolicy = useAssistantActionPolicy("editor.save");
  const [appliedRevision, setAppliedRevision] = useState<number | null>(null);
  const [action, setAction] = useState<ActionState>("idle");
  const [error, setError] = useState<string | null>(null);
  const autoStarted = useRef<string | null>(null);
  const routeRef = useRef(route);
  useEffect(() => {
    routeRef.current = route;
  }, [route]);

  const sessionId = proposal.sessionId;
  const resolved = sessionId ? resolveEditorSession(sessionId) : null;
  const targetStore = resolved?.store;
  const subscribeRevision = useCallback(
    (notify: () => void) => targetStore?.subscribe(notify) ?? (() => {}),
    [targetStore],
  );
  const readRevision = useCallback(
    () => targetStore?.getState()._generation ?? null,
    [targetStore],
  );
  const storeRevision = useSyncExternalStore(
    subscribeRevision,
    readRevision,
    () => null,
  );

  const onEditor = route.startsWith("/editor");
  const stillActive = !!sessionId && isEditorSessionActive(sessionId);
  const expectedRevision = appliedRevision ?? proposal.revision;
  const revisionMatches =
    expectedRevision !== null && storeRevision === expectedRevision;
  const targetStale = !stillActive || !revisionMatches;
  // A live editor tab survives navigation by design. Keep the proposal, but
  // never let an automatic or confirmed action mutate an invisible canvas.
  const unavailable = targetStale || !onEditor;

  const path = resolved?.store.getState().currentFilePath ?? null;
  const readOnly = !!path && isCloud && api.parseBotSourceEditorPath(path) === null;
  const persistable = !!path && !readOnly;
  // Intent is model-reported but never grants authority: it can only select
  // the operator's preconfigured branch. Unknown/legacy intent is non-explicit
  // and therefore still asks under the "explicit" policy.
  const applyExplicit = proposal.applyIntent === "explicit";
  const applyDecision = decideAssistantAction(applyPolicy, applyExplicit);
  const saveExplicit = proposal.saveIntent === "explicit";
  const saveRequested = proposal.saveIntent !== "none";
  const saveDecision = decideAssistantAction(savePolicy, saveExplicit);

  const saveApplied = useCallback(
    async (proposalSession: string, generation: number): Promise<void> => {
      const current = resolveEditorSession(proposalSession);
      if (
        !current ||
        !isEditorSessionActive(proposalSession) ||
        !routeRef.current.startsWith("/editor") ||
        current.store.getState()._generation !== generation
      ) {
        throw new Error(
          "The editor changed before it could be saved. The proposed change remains in the buffer.",
        );
      }
      const state = current.store.getState();
      const currentPath = state.currentFilePath;
      if (!currentPath) {
        throw new Error(
          "This buffer has no file yet. Use Save As in the editor to choose its location.",
        );
      }
      if (isCloud && api.parseBotSourceEditorPath(currentPath) === null) {
        throw new Error(
          "This catalog bot is read-only. Duplicate it before saving changes.",
        );
      }
      if (!state.document) throw new Error("The editor document is unavailable.");

      setAction("saving");
      const savedGeneration = state._generation;
      const result = await api.saveFile(currentPath, state.document);
      const afterSave = resolveEditorSession(proposalSession);
      if (
        afterSave &&
        afterSave.store.getState()._generation === savedGeneration
      ) {
        const savedState = afterSave.store.getState();
        savedState.setCurrentSource(result.source);
        savedState.markSaved();
        setAction("saved");
      } else {
        setAction("applied");
        addToast("Saved, but newer editor changes remain unsaved", "warning");
      }
      pushRecent(currentPath);
    },
    [addToast, isCloud, pushRecent],
  );

  const applyProposal = useCallback(
    async (saveAfter: boolean) => {
      if (!proposal.source || !proposal.sessionId || proposal.revision === null) return;
      if (applyDecision === "deny") {
        setError("Applying assistant changes is disabled in Settings → Assistant.");
        setAction("error");
        return;
      }
      if (saveAfter && saveDecision === "deny") {
        setError("Saving assistant changes is disabled in Settings → Assistant.");
        setAction("error");
        return;
      }
      const current = resolveEditorSession(proposal.sessionId);
      if (
        !current ||
        !routeRef.current.startsWith("/editor") ||
        !isEditorSessionActive(proposal.sessionId) ||
        current.store.getState()._generation !== proposal.revision
      ) {
        setError(
          "The editor tab, page, or document revision changed. Return to the captured buffer and ask again if needed.",
        );
        setAction("error");
        return;
      }
      if (saveAfter) {
        const currentPath = current.store.getState().currentFilePath;
        if (!currentPath) {
          setError("This buffer has no file yet. Use Save As in the editor to choose its location.");
          setAction("error");
          return;
        }
        if (isCloud && api.parseBotSourceEditorPath(currentPath) === null) {
          setError("This catalog bot is read-only. Duplicate it before saving changes.");
          setAction("error");
          return;
        }
      }

      setAction("applying");
      setError(null);
      try {
        const parsed = await api.parseSource(proposal.source);
        const validated = await api.validate(parsed.document);
        const diagnostics = [
          ...(parsed.diagnostics ?? []),
          ...(validated.diagnostics ?? []),
        ];
        if (diagnostics.length > 0) {
          throw new Error(
            `The proposal has ${diagnostics.length} validation error${diagnostics.length === 1 ? "" : "s"}.`,
          );
        }

        // Parsing/validation is asynchronous. Re-check afterwards so a late
        // result cannot clobber an edit the operator made meanwhile.
        const afterValidation = resolveEditorSession(proposal.sessionId);
        if (
          !afterValidation ||
          !routeRef.current.startsWith("/editor") ||
          !isEditorSessionActive(proposal.sessionId) ||
          afterValidation.store.getState()._generation !== proposal.revision
        ) {
          throw new Error(
            "The editor changed while the proposal was being validated. Nothing was applied.",
          );
        }
        const state = afterValidation.store.getState();
        state.setDocument(parsed.document);
        const after = afterValidation.store.getState();
        after.setCurrentSource(proposal.source);
        after.setDiagnostics(
          validated.diagnostics ?? [],
          validated.warnings ?? [],
          validated.issues ?? parsed.issues ?? [],
        );
        const generation = afterValidation.store.getState()._generation;
        setAppliedRevision(generation);

        if (saveAfter) {
          await saveApplied(proposal.sessionId, generation);
        } else {
          setAction("applied");
          addToast(
            "Assistant change applied to the editor — not saved yet",
            "success",
          );
        }
      } catch (err) {
        const message =
          err instanceof Error ? err.message : "Could not apply the proposal";
        setError(message);
        setAction("error");
      }
    },
    [
      proposal,
      applyDecision,
      saveDecision,
      isCloud,
      saveApplied,
      addToast,
    ],
  );

  const save = useCallback(async () => {
    if (!proposal.sessionId || appliedRevision === null) return;
    if (saveDecision === "deny") {
      setError("Saving assistant changes is disabled in Settings → Assistant.");
      setAction("error");
      return;
    }
    setError(null);
    try {
      await saveApplied(proposal.sessionId, appliedRevision);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Save failed");
      setAction("error");
    }
  }, [proposal.sessionId, appliedRevision, saveDecision, saveApplied]);

  // Auto execution is a policy decision, not a model shortcut. The ref makes
  // React StrictMode/remount churn idempotent within this mounted offer; the
  // revision guard is the durable second line of defence.
  useEffect(() => {
    if (
      !proposal.source ||
      !proposal.sessionId ||
      proposal.revision === null ||
      unavailable ||
      action !== "idle" ||
      applyDecision !== "auto"
    ) {
      return;
    }
    const key = `${proposal.sessionId}:${proposal.revision}`;
    if (autoStarted.current === key) return;
    autoStarted.current = key;
    const autoSave =
      saveRequested && saveDecision === "auto" && persistable;
    void applyProposal(autoSave);
  }, [
    proposal,
    unavailable,
    action,
    applyDecision,
    saveRequested,
    saveDecision,
    persistable,
    applyProposal,
  ]);

  if (!proposal.source || !proposal.sessionId || proposal.revision === null) {
    return null;
  }

  const hasApplied = appliedRevision !== null;
  const needsReturn = !onEditor || activeTabId !== resolved?.tabId;
  const canApply =
    !unavailable && applyDecision !== "deny" && action !== "applying";
  const canSave =
    hasApplied &&
    action !== "saved" &&
    !unavailable &&
    persistable &&
    saveDecision !== "deny";
  const returnToEditor = () => {
    if (!resolved) return;
    useTabsStore.getState().setActive(resolved.tabId);
    setLocation(
      path ? `/editor?file=${encodeURIComponent(path)}` : "/editor",
    );
  };

  let detail = "Apply changes only the live buffer; you can undo or save afterwards.";
  if (action === "saved") detail = path ?? "Saved";
  else if (needsReturn) detail = "Return to the captured editor tab to review or run this action.";
  else if (!revisionMatches) detail = "The document changed since this proposal was created.";
  else if (applyDecision === "deny") detail = "Applying assistant changes is disabled in Settings → Assistant.";
  else if (action === "applying") detail = "Validating and applying the proposed bot…";
  else if (action === "saving") detail = "Saving the validated buffer to its existing file…";
  else if (action === "applied") {
    detail = path
      ? "Applied to the live buffer. Nothing has been written to disk."
      : "Applied to the live buffer. Use Save As in the editor to choose a location.";
  } else if (applyDecision === "auto") {
    detail = "Authorized by your Assistant settings. Validation will run before application.";
  }

  return (
    <div className="mt-3 rounded-md border border-border-subtle bg-surface-2 p-2.5">
      <div className="flex items-start gap-2">
        {action === "saved" ? (
          <CheckIcon className="mt-0.5 h-4 w-4 text-success-fg" aria-hidden="true" />
        ) : (
          <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 text-accent-text" aria-hidden="true" />
        )}
        <div className="min-w-0 flex-1">
          <p className="text-label font-medium">
            {action === "saved" ? "Editor change saved" : "Proposed editor change"}
          </p>
          <p className="mt-0.5 text-caption text-fg-muted">{detail}</p>
          {error && <p className="mt-1 text-caption text-danger-fg">{error}</p>}
          <div className="mt-2 flex flex-wrap gap-2">
            {needsReturn && resolved && action !== "saved" && (
              <Button variant="secondary" size="sm" onClick={returnToEditor}>
                Return to the bot
              </Button>
            )}
            {!hasApplied &&
              action !== "saved" &&
              applyDecision === "confirm" && (
                <>
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={!canApply}
                    onClick={() => void applyProposal(false)}
                  >
                    {action === "applying" ? "Validating…" : "Apply to editor"}
                  </Button>
                  {persistable && saveDecision !== "deny" && (
                    <Button
                      variant="primary"
                      size="sm"
                      disabled={!canApply}
                      onClick={() => void applyProposal(true)}
                    >
                      Apply and save
                    </Button>
                  )}
                </>
              )}
            {hasApplied && action !== "saved" && saveDecision !== "deny" && (
              <Button
                variant="primary"
                size="sm"
                disabled={!canSave || action === "saving"}
                onClick={() => void save()}
              >
                {action === "saving" ? "Saving…" : "Save current file"}
              </Button>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
