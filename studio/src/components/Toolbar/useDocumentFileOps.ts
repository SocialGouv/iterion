// useDocumentFileOps groups the 11 document/file action handlers that
// the Toolbar used to inline. The hook owns the local UI state they
// share (Save-as dialog draft + remove-workflow confirm flag) and
// returns the handlers plus the state, so the Toolbar can stay focused
// on layout. Behaviour is identical to the pre-extract code path: the
// same toast text, the same store mutations, the same recents pruning,
// the same Ctrl+S fast-path to currentFilePath.
//
// The hook intentionally takes the `confirm` from useConfirm() and the
// addToast / file-input ref from the caller — those primitives are
// owned by the Toolbar render tree (the confirm dialog node sits in
// JSX, the hidden file input attaches the ref) so the hook stays a
// pure orchestrator.

import { useCallback, useState } from "react";

import * as api from "@/api/client";
import { useDocumentStore } from "@/store/document";
import { useRecentsStore } from "@/store/recents";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import { createEmptyDocument } from "@/lib/defaults";
import { downloadBlob } from "@/lib/download";
import { DISCARD_CHANGES_PROMPT } from "@/lib/copy";
import { errorMessage, toastError } from "@/lib/errorHints";
import { openExampleIntoStore } from "@/lib/openExample";

import type { ConfirmOptions } from "@/hooks/useConfirm";

export interface UseDocumentFileOpsArgs {
  // Promise-based confirm from useConfirm() — the hook needs it for the
  // dirty-tree discard prompt before destructive opens.
  confirm: (options: ConfirmOptions) => Promise<boolean>;
}

export interface UseDocumentFileOpsResult {
  // True when the current file can't be saved from here: cloud mode with a
  // non-botsource (read-only catalog) file. The toolbar disables Save.
  readOnly: boolean;
  // Loading flag for the open/import path. Surfaced as the spinner
  // pill in the toolbar.
  loading: boolean;
  // Save-As dialog state — the file-name input draft plus its open
  // flag. Lifted into the hook because handleSave and
  // handleSaveAsRequest both seed it; the Toolbar renders the Dialog.
  showSaveDialog: boolean;
  setShowSaveDialog: (open: boolean) => void;
  saveFileName: string;
  setSaveFileName: (name: string) => void;
  // Two-step confirm for the workflow-remove IconButton. Kept here
  // because handleRemoveWorkflow is the only place that consumes it.
  confirmRemoveWorkflow: boolean;
  setConfirmRemoveWorkflow: (open: boolean) => void;
  // Handlers.
  handleNew: () => Promise<void>;
  handlePickFile: (kind: "file" | "example", path: string) => Promise<void>;
  handleImport: (e: React.ChangeEvent<HTMLInputElement>) => Promise<void>;
  handleValidate: () => Promise<void>;
  handleSave: () => Promise<void>;
  handleSaveAsRequest: () => void;
  handleSaveAs: () => Promise<void>;
  handleDownload: () => Promise<void>;
  handleCopySource: () => Promise<void>;
  handleAddWorkflow: () => void;
  handleRemoveWorkflow: () => void;
}

export function useDocumentFileOps({
  confirm,
}: UseDocumentFileOpsArgs): UseDocumentFileOpsResult {
  // Document/UI/recents stores — selected one-at-a-time so the hook
  // only re-runs when the slices it actually depends on change.
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const document = useDocumentStore((s) => s.document);
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const setCurrentFilePath = useDocumentStore((s) => s.setCurrentFilePath);
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
  const markSaved = useDocumentStore((s) => s.markSaved);
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  // In cloud there is no writable filesystem: only a team-authored bot (opened
  // under a botsource:// path) can be saved. Any other open file — a baked
  // catalog bot at /opt/iterion/bots, a deep link — is read-only; saving it
  // would 500 with "permission denied". Such a bot must be forked first
  // ("Duplicate & edit"). Local mode always writes to disk.
  const readOnly =
    isCloud && !!currentFilePath && api.parseBotSourceEditorPath(currentFilePath) === null;
  const READ_ONLY_MSG =
    "This is a read-only catalog bot. Use “Duplicate & edit” on the bot's page to make an editable copy.";
  const isDirty = useDocumentStore((s) => s.isDirty);
  const addWorkflow = useDocumentStore((s) => s.addWorkflow);
  const removeWorkflow = useDocumentStore((s) => s.removeWorkflow);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);
  const setActiveWorkflowName = useUIStore((s) => s.setActiveWorkflowName);
  const addToast = useUIStore((s) => s.addToast);
  const openDiagnosticsPanel = useUIStore((s) => s.openDiagnosticsPanel);
  const pushRecent = useRecentsStore((s) => s.pushRecent);
  const removeRecent = useRecentsStore((s) => s.removeRecent);

  const [loading, setLoading] = useState(false);
  const [showSaveDialog, setShowSaveDialog] = useState(false);
  const [saveFileName, setSaveFileName] = useState("");
  const [confirmRemoveWorkflow, setConfirmRemoveWorkflow] = useState(false);

  const confirmDiscard = useCallback(async () => {
    if (!isDirty()) return true;
    return confirm(DISCARD_CHANGES_PROMPT);
  }, [isDirty, confirm]);

  const handleNew = useCallback(async () => {
    if (!(await confirmDiscard())) return;
    setDocument(createEmptyDocument());
    setDiagnostics([], []);
    setCurrentFilePath(null);
    setCurrentSource(null);
    markSaved();
  }, [
    setDocument,
    setDiagnostics,
    setCurrentFilePath,
    setCurrentSource,
    markSaved,
    confirmDiscard,
  ]);

  const handlePickFile = useCallback(
    async (kind: "file" | "example", path: string) => {
      if (!(await confirmDiscard())) return;
      setLoading(true);
      try {
        if (kind === "file") {
          const result = await api.openFile(path);
          setDocument(result.document);
          setDiagnostics(result.diagnostics);
          setCurrentFilePath(result.path);
          setCurrentSource(result.source);
          pushRecent(result.path);
          markSaved();
        } else {
          // Productised bots live at <WorkDir>/bots/<name>; the shared
          // helper binds that path (so Save works and the Run button
          // enables instead of "Save the workflow first") and keeps the
          // example's source + diagnostics. Same path as RecentFilesPanel
          // and CanvasEmpty.
          await openExampleIntoStore(path, {
            setDocument,
            setDiagnostics,
            setCurrentSource,
            setCurrentFilePath,
            markSaved,
          });
        }
      } catch (err) {
        console.error("Open failed:", err);
        // Auto-clean stale recents: a 404 from /files/open means the
        // file underlying this recent entry no longer exists (deleted,
        // moved, or the workspace was switched to a project that
        // doesn't contain it). Pruning here means the next time the
        // picker opens, the dead row is gone — instead of the user
        // having to manually click the trash icon on every stale row.
        const message = errorMessage(err) ?? "";
        const isMissing = /file not found|no such file|404/i.test(message);
        if (kind === "file" && isMissing) {
          removeRecent(path);
          addToast(`Removed missing file from recents: ${path}`, "warning");
        } else {
          addToast("Open failed", "error");
        }
      } finally {
        setLoading(false);
      }
    },
    [
      setDocument,
      setDiagnostics,
      setCurrentFilePath,
      setCurrentSource,
      markSaved,
      confirmDiscard,
      pushRecent,
      removeRecent,
      addToast,
    ],
  );

  const handleImport = useCallback(
    async (e: React.ChangeEvent<HTMLInputElement>) => {
      const file = e.target.files?.[0];
      if (!file) return;
      if (!file.name.endsWith(".bot")) {
        addToast("Only .bot files can be imported", "error");
        e.target.value = "";
        return;
      }
      if (!(await confirmDiscard())) {
        e.target.value = "";
        return;
      }
      const text = await file.text();
      try {
        const result = await api.parseSource(text);
        setDocument(result.document);
        setDiagnostics(result.diagnostics);
        setCurrentFilePath(null);
        // Imported files are off-disk; the original text is the source.
        setCurrentSource(text);
      } catch (err) {
        console.error("Import failed:", err);
        toastError(addToast, err, "Import failed");
      }
      e.target.value = "";
    },
    [
      setDocument,
      setDiagnostics,
      setCurrentFilePath,
      setCurrentSource,
      confirmDiscard,
      addToast,
    ],
  );

  const handleValidate = useCallback(async () => {
    if (!document) return;
    try {
      const result = await api.validate(document);
      setDiagnostics(result.diagnostics, result.warnings, result.issues);
      const errorCount = (result.diagnostics ?? []).length;
      const warnCount = (result.warnings ?? []).length;
      if (errorCount === 0 && warnCount === 0) {
        addToast("No issues found", "success");
      } else {
        addToast(
          `${errorCount} error${errorCount !== 1 ? "s" : ""}, ${warnCount} warning${warnCount !== 1 ? "s" : ""}`,
          "error",
        );
        // Surface the detail, not just the count — pop the Diagnostics panel.
        openDiagnosticsPanel();
      }
    } catch (err) {
      console.error("Validation failed:", err);
      addToast(`Validation failed: ${errorMessage(err)}`, "error");
    }
  }, [document, setDiagnostics, addToast, openDiagnosticsPanel]);

  const handleSave = useCallback(async () => {
    if (!document) return;
    if (readOnly) {
      addToast(READ_ONLY_MSG, "warning");
      return;
    }
    if (currentFilePath) {
      try {
        const result = await api.saveFile(currentFilePath, document);
        setCurrentSource(result.source);
        markSaved();
        addToast("Saved successfully", "success");
        pushRecent(currentFilePath);
      } catch (err) {
        console.error("Save failed:", err);
        addToast("Save failed", "error");
      }
    } else {
      const name = document.workflows?.[0]?.name || "workflow";
      setSaveFileName(`${name}.bot`);
      setShowSaveDialog(true);
    }
  }, [
    document,
    currentFilePath,
    setCurrentSource,
    markSaved,
    addToast,
    pushRecent,
    readOnly,
    READ_ONLY_MSG,
  ]);

  // Always opens the Save As dialog regardless of whether a file path
  // is already bound — distinct from handleSave which fast-paths to the
  // current path when one exists.
  const handleSaveAsRequest = useCallback(() => {
    if (!document) return;
    const fallback = document.workflows?.[0]?.name || "workflow";
    const seed = currentFilePath
      ? currentFilePath.split("/").pop() || `${fallback}.bot`
      : `${fallback}.bot`;
    setSaveFileName(seed);
    setShowSaveDialog(true);
  }, [document, currentFilePath]);

  const handleSaveAs = useCallback(async () => {
    if (!document || !saveFileName) return;
    if (isCloud) {
      // Save As targets a filesystem path, which cloud can't write. Route bot
      // creation through the Bots page (/bots/new) or "Duplicate & edit".
      addToast(
        "Save As isn't available in cloud — create a bot from the Bots page, or Duplicate & edit an existing one.",
        "warning",
      );
      setShowSaveDialog(false);
      return;
    }
    const fileName = saveFileName.endsWith(".bot") ? saveFileName : `${saveFileName}.bot`;
    try {
      const result = await api.saveFile(fileName, document);
      setCurrentFilePath(result.path);
      setCurrentSource(result.source);
      markSaved();
      pushRecent(result.path);
      addToast("Saved successfully", "success");
      setShowSaveDialog(false);
    } catch (err) {
      console.error("Save failed:", err);
      addToast("Save failed", "error");
    }
  }, [
    document,
    saveFileName,
    setCurrentFilePath,
    setCurrentSource,
    markSaved,
    pushRecent,
    addToast,
    isCloud,
    setShowSaveDialog,
  ]);

  const handleDownload = useCallback(async () => {
    if (!document) return;
    try {
      const source = await api.unparse(document);
      const blob = new Blob([source], { type: "text/plain" });
      const name = document.workflows?.[0]?.name || "workflow";
      downloadBlob(blob, `${name}.bot`);
    } catch (err) {
      console.error("Download failed:", err);
      addToast("Download failed", "error");
    }
  }, [document, addToast]);

  const handleCopySource = useCallback(async () => {
    if (!document) return;
    try {
      const source = await api.unparse(document);
      await navigator.clipboard.writeText(source);
      addToast("Source copied to clipboard", "success");
    } catch (err) {
      console.error("Copy failed:", err);
      addToast("Copy failed", "error");
    }
  }, [document, addToast]);

  const handleAddWorkflow = useCallback(() => {
    if (!document) return;
    const existing = new Set(document.workflows.map((w) => w.name));
    let i = 1;
    while (existing.has(`workflow_${i}`)) i++;
    const name = `workflow_${i}`;
    addWorkflow({ name, entry: "", edges: [] });
    setActiveWorkflowName(name);
  }, [document, addWorkflow, setActiveWorkflowName]);

  const handleRemoveWorkflow = useCallback(() => {
    if (!document || !activeWorkflowName) return;
    if (document.workflows.length <= 1) return;
    removeWorkflow(activeWorkflowName);
    setActiveWorkflowName(null);
  }, [document, activeWorkflowName, removeWorkflow, setActiveWorkflowName]);

  return {
    readOnly,
    loading,
    showSaveDialog,
    setShowSaveDialog,
    saveFileName,
    setSaveFileName,
    confirmRemoveWorkflow,
    setConfirmRemoveWorkflow,
    handleNew,
    handlePickFile,
    handleImport,
    handleValidate,
    handleSave,
    handleSaveAsRequest,
    handleSaveAs,
    handleDownload,
    handleCopySource,
    handleAddWorkflow,
    handleRemoveWorkflow,
  };
}
