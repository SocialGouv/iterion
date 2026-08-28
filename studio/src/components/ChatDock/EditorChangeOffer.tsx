// A validated assistant proposal becomes a studio action, never a file tool.
// Apply replaces only the still-active editor buffer at the captured revision;
// Save is a second, explicit operator click and reuses the tab's bound path.

import { useCallback, useState, useSyncExternalStore } from "react";
import { CheckIcon, ExclamationTriangleIcon } from "@radix-ui/react-icons";

import * as api from "@/api/client";
import { Button } from "@/components/ui/Button";
import { useEditorProposal } from "@/hooks/useEditorProposal";
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
  const activeTabId = useTabsStore((state) => state.activeEditorTabId);
  const isCloud = useServerInfoStore((state) => state.info?.mode === "cloud");
  const addToast = useUIStore((state) => state.addToast);
  const pushRecent = useRecentsStore((state) => state.pushRecent);
  const [appliedRevision, setAppliedRevision] = useState<number | null>(null);
  const [action, setAction] = useState<ActionState>("idle");
  const [error, setError] = useState<string | null>(null);

  const sessionId = proposal.sessionId;
  const targetStore = sessionId ? resolveEditorSession(sessionId)?.store : undefined;
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

  const stillActive = !!sessionId && isEditorSessionActive(sessionId);
  const expectedRevision = appliedRevision ?? proposal.revision;
  const revisionMatches =
    expectedRevision !== null && storeRevision === expectedRevision;
  const stale = !stillActive || !revisionMatches;

  const apply = useCallback(async () => {
    if (!proposal.source || !proposal.sessionId || proposal.revision === null) return;
    const resolved = resolveEditorSession(proposal.sessionId);
    if (
      !resolved ||
      !isEditorSessionActive(proposal.sessionId) ||
      resolved.store.getState()._generation !== proposal.revision
    ) {
      setError("The editor tab or document revision changed. Ask the assistant again from the current buffer.");
      setAction("error");
      return;
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
        throw new Error(`The proposal has ${diagnostics.length} validation error${diagnostics.length === 1 ? "" : "s"}.`);
      }

      // Parsing/validation is asynchronous. Re-check the capability after it:
      // applying to a buffer the operator edited meanwhile would be a clobber.
      const current = resolveEditorSession(proposal.sessionId);
      if (
        !current ||
        !isEditorSessionActive(proposal.sessionId) ||
        current.store.getState()._generation !== proposal.revision
      ) {
        throw new Error("The editor changed while the proposal was being validated. Nothing was applied.");
      }
      const state = current.store.getState();
      state.setDocument(parsed.document);
      const after = current.store.getState();
      after.setCurrentSource(proposal.source);
      after.setDiagnostics(
        validated.diagnostics ?? [],
        validated.warnings ?? [],
        validated.issues ?? parsed.issues ?? [],
      );
      setAppliedRevision(current.store.getState()._generation);
      setAction("applied");
      addToast("Assistant change applied to the editor — not saved yet", "success");
    } catch (err) {
      const message = err instanceof Error ? err.message : "Could not apply the proposal";
      setError(message);
      setAction("error");
    }
  }, [proposal, addToast]);

  const save = useCallback(async () => {
    if (!proposal.sessionId || appliedRevision === null) return;
    const resolved = resolveEditorSession(proposal.sessionId);
    if (
      !resolved ||
      !isEditorSessionActive(proposal.sessionId) ||
      resolved.store.getState()._generation !== appliedRevision
    ) {
      setError("The active buffer changed after Apply. Review it and use the editor's Save action.");
      setAction("error");
      return;
    }
    const state = resolved.store.getState();
    const path = state.currentFilePath;
    if (!path) {
      setError("This buffer has no file yet. Use Save As in the editor to choose its location.");
      setAction("error");
      return;
    }
    if (isCloud && api.parseBotSourceEditorPath(path) === null) {
      setError("This catalog bot is read-only. Duplicate it before saving changes.");
      setAction("error");
      return;
    }
    if (!state.document) return;

    setAction("saving");
    setError(null);
    try {
      const savedGeneration = state._generation;
      const result = await api.saveFile(path, state.document);
      const current = resolveEditorSession(proposal.sessionId);
      if (
        current &&
        current.store.getState()._generation === savedGeneration
      ) {
        const currentState = current.store.getState();
        currentState.setCurrentSource(result.source);
        currentState.markSaved();
        setAction("saved");
      } else {
        setAction("applied");
        addToast("Saved, but newer editor changes remain unsaved", "warning");
      }
      pushRecent(path);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Save failed");
      setAction("error");
    }
  }, [proposal.sessionId, appliedRevision, isCloud, addToast, pushRecent]);

  if (!proposal.source || !proposal.sessionId || proposal.revision === null) {
    return null;
  }

  const resolved = resolveEditorSession(proposal.sessionId);
  const path = resolved?.store.getState().currentFilePath ?? null;
  const readOnly = !!path && isCloud && api.parseBotSourceEditorPath(path) === null;
  const hasApplied = appliedRevision !== null;
  const canSave =
    hasApplied &&
    action !== "saved" &&
    !stale &&
    !!path &&
    !readOnly;

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
          <p className="mt-0.5 text-caption text-fg-muted">
            {stale && action !== "saved"
              ? activeTabId === resolved?.tabId
                ? "The document changed since this proposal was created."
                : "Return to the editor tab where this proposal was requested."
              : action === "applied"
                ? path
                  ? "Applied to the live buffer. Nothing has been written to disk."
                  : "Applied to the live buffer. Use Save As in the editor to choose a location."
                : action === "saved"
                  ? path ?? "Saved"
                  : "Apply changes only the live buffer; you can undo or save afterwards."}
          </p>
          {error && <p className="mt-1 text-caption text-danger-fg">{error}</p>}
          <div className="mt-2 flex flex-wrap gap-2">
            {!hasApplied && action !== "saved" && (
              <Button
                variant="secondary"
                size="sm"
                disabled={stale || action === "applying"}
                onClick={() => void apply()}
              >
                {action === "applying" ? "Validating…" : "Apply to editor"}
              </Button>
            )}
            {hasApplied && action !== "saved" && (
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
