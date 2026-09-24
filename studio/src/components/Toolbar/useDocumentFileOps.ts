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
import {
  useDocumentStore,
  useDocumentStoreInstance,
} from "@/store/document";
import { useRecentsStore } from "@/store/recents";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import { useDocumentSaveAs } from "@/components/DocumentSaveAs/useDocumentSaveAs";
import { createEmptyDocument } from "@/lib/defaults";
import { downloadBlob } from "@/lib/download";
import { DISCARD_CHANGES_PROMPT } from "@/lib/copy";
import { errorMessage, toastError } from "@/lib/errorHints";
import { openExampleIntoStore } from "@/lib/openExample";
import { applyParsedSource, salvageRefusal } from "@/lib/salvage";
import { isSharedBundleFilePath } from "@/lib/sharedBundle";

import type { ConfirmOptions } from "@/hooks/useConfirm";
import type { DocumentSaveAsController } from "@/components/DocumentSaveAs/useDocumentSaveAs";

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
  // Shared Save As controller used by both the toolbar and Copi offers.
  saveAs: DocumentSaveAsController;
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
  handleDownload: () => Promise<void>;
  handleCopySource: () => Promise<void>;
  handleAddWorkflow: () => void;
  handleRemoveWorkflow: () => void;
}
import { applyOpenedFile } from "@/lib/openedFile";
import { ReplaceDeadlineError, replaceDocument, replaceDocumentNow } from "@/lib/replaceDocument";
import { offerReload } from "@/lib/reloadOffer";
import { stampEditor, stampHolds } from "@/store/document";

export function useDocumentFileOps({
  confirm,
}: UseDocumentFileOpsArgs): UseDocumentFileOpsResult {
  // Document/UI/recents stores — selected one-at-a-time so the hook
  // only re-runs when the slices it actually depends on change.
  const documentStore = useDocumentStoreInstance();
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const document = useDocumentStore((s) => s.document);
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
  const unit = useDocumentStore((s) => s.unit);
  const setUnit = useDocumentStore((s) => s.setUnit);
  const markSaved = useDocumentStore((s) => s.markSaved);
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  // In cloud there is no writable filesystem: only a team-authored bot (opened
  // under a botsource:// path) can be saved. Any other open file — a baked
  // catalog bot at /opt/iterion/bots, a deep link — is read-only; saving it
  // would 500 with "permission denied". Such a bot must be forked first
  // ("Duplicate & edit"). Local mode always writes to disk.
  const sharedBundle = isSharedBundleFilePath(currentFilePath);
  const readOnly =
    sharedBundle ||
    (isCloud && !!currentFilePath && api.parseBotSourceEditorPath(currentFilePath) === null);
  const READ_ONLY_MSG = sharedBundle
    ? "This workflow comes from a locked shared bundle. Edit its source bundle, update bots.lock, then run iterion bots sync."
    : "This is a read-only catalog bot. Use “Duplicate & edit” on the bot's page to make an editable copy.";
  const hasUnsavedWork = useDocumentStore((s) => s.hasUnsavedWork);
  const addWorkflow = useDocumentStore((s) => s.addWorkflow);
  const removeWorkflow = useDocumentStore((s) => s.removeWorkflow);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);
  const setActiveWorkflowName = useUIStore((s) => s.setActiveWorkflowName);
  const addToast = useUIStore((s) => s.addToast);
  const openDiagnosticsPanel = useUIStore((s) => s.openDiagnosticsPanel);
  const pushRecent = useRecentsStore((s) => s.pushRecent);
  const removeRecent = useRecentsStore((s) => s.removeRecent);
  const saveAs = useDocumentSaveAs();

  const [loading, setLoading] = useState(false);
  const [confirmRemoveWorkflow, setConfirmRemoveWorkflow] = useState(false);

  // `hasUnsavedWork`, not `isDirty`: the Source view's un-applied text is
  // work this discard takes too, and it moves no document generation (#1662).
  const confirmDiscard = useCallback(async () => {
    if (!hasUnsavedWork()) return true;
    return confirm(DISCARD_CHANGES_PROMPT);
  }, [hasUnsavedWork, confirm]);

  const handleNew = useCallback(async () => {
    if (!(await confirmDiscard())) return;
    replaceDocumentNow(documentStore, (s) => {
      s.setDocument(createEmptyDocument());
      s.setDiagnostics([], []);
      s.setCurrentFilePath(null);
      s.setCurrentSource(null);
      s.markSaved();
    });
  }, [documentStore, confirmDiscard]);

  const handlePickFile = useCallback(
    async (kind: "file" | "example", path: string) => {
      if (!(await confirmDiscard())) return;
      setLoading(true);
      try {
        if (kind === "file") {
          await replaceDocument(documentStore, path, (signal) => api.openFile(path, { signal }), (result, s) => {
            applyOpenedFile(result, s);
            if (result.path) pushRecent(result.path);
          });
        } else {
          // The shared helper binds the path the server names for a file
          // inside the workspace, else bots/<name> (so Save works and Run
          // launches it by path), and keeps the example's source +
          // diagnostics. Same path as RecentFilesPanel and CanvasEmpty.
          await openExampleIntoStore(path, documentStore);
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
        const isMissing = !(err instanceof ReplaceDeadlineError) && /file not found|no such file|404/i.test(message);
        if (kind === "file" && isMissing) {
          removeRecent(path);
          addToast(`Removed missing file from recents: ${path}`, "warning");
        } else {
          toastError(addToast, err, "Open failed");
        }
      } finally {
        setLoading(false);
      }
    },
    [documentStore, confirmDiscard, pushRecent, removeRecent, addToast],
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
      try {
        // The file is read inside the load: the confirm was answered for the
        // work present NOW, and an edit made while the file is read or
        // parsed is work nobody was asked about.
        await replaceDocument(
          documentStore,
          file.name,
          async (signal) => {
            const text = await file.text();
            return { text, result: await api.parseSource(text, { signal }) };
          },
          ({ text, result }, s) => {
            s.setDiagnostics(result.diagnostics);
            // The path first — it clears the salvage flag — then the document
            // and the verdict together. Unbinding does NOT protect an import:
            // Save As is the only write an unbound buffer offers, and it would
            // put a file missing what the parser could not read under the name
            // the author chose.
            s.setCurrentFilePath(null);
            applyParsedSource(result, s);
            // Imported files are off-disk; the original text is the source.
            s.setCurrentSource(text);
          },
        );
      } catch (err) {
        console.error("Import failed:", err);
        toastError(addToast, err, "Import failed");
      }
      e.target.value = "";
    },
    [documentStore, confirmDiscard, addToast],
  );

  const handleValidate = useCallback(async () => {
    if (!document) return;
    // The diagnostics describe THIS document of THIS file: an answer that
    // lands after either moved is about something no longer on screen.
    const asked = stampEditor(documentStore.getState());
    try {
      const result = await api.validate(document, undefined, currentFilePath);
      const holds = stampHolds(asked, documentStore.getState());
      if (!holds.path || !holds.generation) {
        addToast("The editor changed while validating, so the result was not shown. Validate again.", "warning");
        return;
      }
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
  }, [document, currentFilePath, documentStore, setDiagnostics, addToast, openDiagnosticsPanel]);

  const handleSave = useCallback(async () => {
    if (!document) return;
    if (readOnly) {
      addToast(READ_ONLY_MSG, "warning");
      return;
    }
    const refusal = salvageRefusal(documentStore.getState());
    if (refusal) {
      addToast(refusal, "warning", { persistent: true });
      openDiagnosticsPanel();
      return;
    }
    if (currentFilePath) {
      const path = currentFilePath;
      const asked = stampEditor(documentStore.getState());
      try {
        // A bot in several files presents the revision it was opened at,
        // and keeps the one the save returns.
        const result = await api.saveFile(path, document, unit ? { revision: unit.revision } : undefined);
        pushRecent(path);
        // The answer is about the document it wrote. A tab that has moved to
        // another file meanwhile — or reopened this same one, which is a new
        // document under the same name — takes none of it: not this write's
        // source, not its unit revision, not a "saved" mark over work it did
        // not write. A replacement ASKED for and not applied (it failed, or
        // was refused) left the written document on screen, and does not
        // stop it being settled.
        const now = documentStore.getState();
        const holds = stampHolds(asked, now);
        if (!holds.path || !holds.replaced) {
          // Reopened under the same name while the write was in flight: a
          // reading taken before the write shows the file as it no longer
          // is, marked saved, and the next save would write it back.
          if (holds.path && now.currentSource !== result.source) {
            offerReload(documentStore, path, `Saved ${path} — the tab was reopened meanwhile and does not show what was saved.`);
          } else {
            addToast(`Saved ${path}`, "success");
          }
          return;
        }
        setCurrentSource(result.source);
        // The revision goes onto the unit as it is NOW: a per-file Apply
        // during the write may have changed its files.
        if (now.unit) setUnit({ ...now.unit, revision: result.revision ?? now.unit.revision });
        const clean = holds.generation;
        if (clean) markSaved();
        addToast(
          clean ? "Saved successfully" : "Saved, but newer editor changes remain unsaved",
          clean ? "success" : "warning",
        );
      } catch (err) {
        console.error("Save failed:", err);
        // The server's own sentence: a save refused because the writer
        // cannot reproduce the file (#1612) names the file, the line and
        // what to do. "Save failed" alone leaves the author nowhere.
        toastError(addToast, err, "Save failed", { persistent: true });
      }
    } else {
      saveAs.requestSaveAs({ store: documentStore });
    }
  }, [
    document,
    currentFilePath,
    setCurrentSource,
    unit,
    setUnit,
    markSaved,
    addToast,
    pushRecent,
    readOnly,
    READ_ONLY_MSG,
    saveAs,
    documentStore,
    openDiagnosticsPanel,
  ]);

  // Always opens the Save As dialog regardless of whether a file path
  // is already bound — distinct from handleSave which fast-paths to the
  // current path when one exists.
  const handleSaveAsRequest = useCallback(() => {
    if (!document) return;
    saveAs.requestSaveAs({ store: documentStore });
  }, [document, documentStore, saveAs]);

  const handleDownload = useCallback(async () => {
    if (!document) return;
    // A .bot on the author's disk, under a name they will trust, is the same
    // harm as a save: the salvage is the program minus what the parser could
    // not read, and nothing on the file says so.
    const refusal = salvageRefusal(documentStore.getState());
    if (refusal) {
      addToast(refusal, "warning", { persistent: true });
      openDiagnosticsPanel();
      return;
    }
    try {
      const { source, refused, stored } = await api.unparse(document, {
        // A bot in several files downloads as its PROGRAM: one text, which
        // is what a `.bot` on the author's disk means. The path travels
        // with it so the answer can still name which of the bot's files
        // the writer cannot reproduce.
        ...(unit ? { flatten: true } : {}),
        path: documentStore.getState().currentFilePath,
      });
      // A third shape of the same harm: the writer has no multi-line form
      // for one of this file's values (#1612), so the .bot it would hand
      // over puts on one line what the author wrote over several. Same
      // program, and not the same file.
      if (refused) {
        // Handing over `source` is a better export than refusing — the
        // author gets their file, never a collapsed render of it — but
        // ONLY where `source` is a file: `stored` says so, and the merged
        // answer a bot in several files takes is a render of a program
        // that is no file at all. Reading `refused` alone as "so source is
        // the file" is right three times out of four.
        //
        // The other case it is wrong is a canvas holding edits that text
        // does not carry, which is what isDirty() says.
        if (!stored || documentStore.getState().isDirty()) {
          addToast(
            stored
              ? `This bot cannot be downloaded as .bot source: ${refused}. Save your changes first — the file as stored can be downloaded once the canvas matches it.`
              : `This bot cannot be downloaded as .bot source: ${refused}. A bot in several files has no single file to hand over instead.`,
            "warning",
            { persistent: true },
          );
          return;
        }
        addToast(
          `Downloaded this bot as it is stored: iterion cannot rewrite it (${refused}).`,
          "warning",
          { persistent: true },
        );
      }
      const blob = new Blob([source], { type: "text/plain" });
      const name = document.workflows?.[0]?.name || "workflow";
      downloadBlob(blob, `${name}.bot`);
    } catch (err) {
      console.error("Download failed:", err);
      addToast(err instanceof Error ? `Download failed: ${err.message}` : "Download failed", "error");
    }
  }, [document, addToast, documentStore, openDiagnosticsPanel, unit]);

  const handleCopySource = useCallback(async () => {
    if (!document) return;
    // Same harm, one step removed: the text goes to a file or a message
    // next, and it is the program minus what the parser could not read.
    const refusal = salvageRefusal(documentStore.getState());
    if (refusal) {
      addToast(refusal, "warning", { persistent: true });
      openDiagnosticsPanel();
      return;
    }
    try {
      const { source, refused, stored } = await api.unparse(document, {
        // A bot in several files downloads as its PROGRAM: one text, which
        // is what a `.bot` on the author's disk means. The path travels
        // with it so the answer can still name which of the bot's files
        // the writer cannot reproduce.
        ...(unit ? { flatten: true } : {}),
        path: documentStore.getState().currentFilePath,
      });
      if (refused) {
        // Same as the download: only where `source` is a file.
        if (!stored || documentStore.getState().isDirty()) {
          addToast(
            stored
              ? `This bot cannot be copied as .bot source: ${refused}. Save your changes first — the file as stored can be copied once the canvas matches it.`
              : `This bot cannot be copied as .bot source: ${refused}. A bot in several files has no single file to hand over instead.`,
            "warning",
            { persistent: true },
          );
          return;
        }
        addToast(
          `Copied this bot as it is stored: iterion cannot rewrite it (${refused}).`,
          "warning",
          { persistent: true },
        );
        await navigator.clipboard.writeText(source);
        return;
      }
      await navigator.clipboard.writeText(source);
      addToast("Source copied to clipboard", "success");
    } catch (err) {
      console.error("Copy failed:", err);
      addToast(err instanceof Error ? `Copy failed: ${err.message}` : "Copy failed", "error");
    }
  }, [document, addToast, documentStore, openDiagnosticsPanel, unit]);

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
    saveAs,
    confirmRemoveWorkflow,
    setConfirmRemoveWorkflow,
    handleNew,
    handlePickFile,
    handleImport,
    handleValidate,
    handleSave,
    handleSaveAsRequest,
    handleDownload,
    handleCopySource,
    handleAddWorkflow,
    handleRemoveWorkflow,
  };
}
