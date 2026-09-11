import { useCallback, useRef, useState } from "react";

import * as api from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import type { DocumentStore } from "@/store/document";
import { useRecentsStore } from "@/store/recents";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

export interface DocumentSaveAsResult {
  path: string;
  source: string;
  clean: boolean;
}

export interface DocumentSaveAsRequest {
  store: DocumentStore;
  /** Assistant requests bind Save As to one captured editor generation. */
  expectedGeneration?: number;
  /** Revalidates the editor session/tab at confirmation and after the write. */
  isTargetCurrent?: () => boolean;
  /** Rechecks caller-owned policy or other mutable authorization at confirm. */
  validate?: () => string | null;
  onSaved?: (result: DocumentSaveAsResult) => void;
}

type PendingSaveAs = DocumentSaveAsRequest;

function suggestedFileName(store: DocumentStore): string {
  const state = store.getState();
  const fallback = state.document?.workflows?.[0]?.name || "workflow";
  return state.currentFilePath
    ? state.currentFilePath.split("/").pop() || `${fallback}.bot`
    : `${fallback}.bot`;
}

export function useDocumentSaveAs() {
  const isCloud = useServerInfoStore((state) => state.info?.mode === "cloud");
  const addToast = useUIStore((state) => state.addToast);
  const pushRecent = useRecentsStore((state) => state.pushRecent);
  const pending = useRef<PendingSaveAs | null>(null);
  const [open, setOpenState] = useState(false);
  const [fileName, setFileName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const setOpen = useCallback((next: boolean) => {
    setOpenState(next);
    if (!next) {
      pending.current = null;
      setError(null);
      setBusy(false);
    }
  }, []);

  const requestSaveAs = useCallback(
    (request: DocumentSaveAsRequest): boolean => {
      if (isCloud) {
        addToast(
          "Save As isn't available in cloud — create a bot from the Bots page, or Duplicate & edit an existing one.",
          "warning",
        );
        return false;
      }
      if (!request.store.getState().document) return false;
      pending.current = request;
      setFileName(suggestedFileName(request.store));
      setError(null);
      setOpenState(true);
      return true;
    },
    [addToast, isCloud],
  );

  const confirmSaveAs = useCallback(async () => {
    const target = pending.current;
    const trimmed = fileName.trim();
    if (!target || !trimmed || busy) return;

    const state = target.store.getState();
    const validationError = target.validate?.() ?? null;
    if (validationError) {
      setError(validationError);
      return;
    }
    if (
      target.isTargetCurrent?.() === false ||
      (target.expectedGeneration !== undefined &&
        state._generation !== target.expectedGeneration)
    ) {
      setError(
        "The editor changed since this save was requested. Close this dialog and ask again.",
      );
      return;
    }
    if (!state.document) {
      setError("The editor document is unavailable.");
      return;
    }

    const path = trimmed.endsWith(".bot") ? trimmed : `${trimmed}.bot`;
    const savedGeneration = state._generation;
    const document = state.document;
    setBusy(true);
    setError(null);
    try {
      const result = await api.saveFile(path, document, { createOnly: true });
      const stillCurrent = target.isTargetCurrent?.() !== false;
      const after = target.store.getState();
      const clean = stillCurrent && after._generation === savedGeneration;

      if (stillCurrent) {
        after.setCurrentFilePath(result.path);
        after.setCurrentSource(result.source);
        if (clean) after.markSaved();
      }
      pushRecent(result.path);
      setOpenState(false);
      pending.current = null;
      if (stillCurrent) {
        addToast(
          clean
            ? "Saved successfully"
            : "Saved, but newer editor changes remain unsaved",
          clean ? "success" : "warning",
        );
      } else {
        addToast(
          `Saved ${result.path}, but the original editor session is no longer active`,
          "warning",
        );
      }
      target.onSaved?.({ path: result.path, source: result.source, clean });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }, [addToast, busy, fileName, pushRecent]);

  return {
    open,
    setOpen,
    fileName,
    setFileName,
    busy,
    error,
    requestSaveAs,
    confirmSaveAs,
  };
}

export type DocumentSaveAsController = ReturnType<typeof useDocumentSaveAs>;
