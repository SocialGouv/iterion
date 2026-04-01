import { useCallback, useEffect, useRef, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { createEmptyDocument } from "@/lib/defaults";
import * as api from "@/api/client";
import ConfirmDialog from "../shared/ConfirmDialog";
import ShortcutsHelp from "../shared/ShortcutsHelp";
import type { FileEntry } from "@/api/types";

export default function Toolbar() {
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const document = useDocumentStore((s) => s.document);
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const setCurrentFilePath = useDocumentStore((s) => s.setCurrentFilePath);
  const undo = useDocumentStore((s) => s.undo);
  const redo = useDocumentStore((s) => s.redo);
  const canUndo = useDocumentStore((s) => s.canUndo);
  const canRedo = useDocumentStore((s) => s.canRedo);
  const markSaved = useDocumentStore((s) => s.markSaved);
  const isDirty = useDocumentStore((s) => s.isDirty);
  const addWorkflow = useDocumentStore((s) => s.addWorkflow);
  const removeWorkflow = useDocumentStore((s) => s.removeWorkflow);
  const sourceViewOpen = useUIStore((s) => s.sourceViewOpen);
  const toggleSourceView = useUIStore((s) => s.toggleSourceView);
  const diagnosticsPanelOpen = useUIStore((s) => s.diagnosticsPanelOpen);
  const toggleDiagnosticsPanel = useUIStore((s) => s.toggleDiagnosticsPanel);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);
  const setActiveWorkflowName = useUIStore((s) => s.setActiveWorkflowName);
  const addToast = useUIStore((s) => s.addToast);

  const [examples, setExamples] = useState<string[]>([]);
  const [files, setFiles] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [showSaveDialog, setShowSaveDialog] = useState(false);
  const [saveFileName, setSaveFileName] = useState("");
  const [showOpenMenu, setShowOpenMenu] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const openMenuRef = useRef<HTMLDivElement>(null);
  const [confirmRemoveWorkflow, setConfirmRemoveWorkflow] = useState(false);
  const [showShortcuts, setShowShortcuts] = useState(false);

  useEffect(() => {
    api.listExamples().then(setExamples).catch(console.error);
    api.listFiles().then(setFiles).catch(() => setFiles([]));
  }, []);

  // Close open menu on outside click
  useEffect(() => {
    if (!showOpenMenu) return;
    const handler = (e: MouseEvent) => {
      if (openMenuRef.current && !openMenuRef.current.contains(e.target as Node)) {
        setShowOpenMenu(false);
      }
    };
    window.document.addEventListener("mousedown", handler);
    return () => window.document.removeEventListener("mousedown", handler);
  }, [showOpenMenu]);

  const handleNew = useCallback(() => {
    if (isDirty() && !window.confirm("You have unsaved changes. Discard them?")) return;
    setDocument(createEmptyDocument());
    setDiagnostics([], []);
    setCurrentFilePath(null);
    markSaved();
  }, [setDocument, setDiagnostics, setCurrentFilePath, markSaved, isDirty]);

  const loadExample = useCallback(
    async (name: string) => {
      if (!name) return;
      if (isDirty() && !window.confirm("You have unsaved changes. Discard them?")) return;
      setLoading(true);
      try {
        const result = await api.loadExample(name);
        setDocument(result.document);
        setDiagnostics(result.diagnostics);
        setCurrentFilePath(null);
        markSaved();
      } catch (err) {
        console.error("Failed to load example:", err);
      } finally {
        setLoading(false);
      }
    },
    [setDocument, setDiagnostics, setCurrentFilePath, markSaved, isDirty],
  );

  const handleOpenFile = useCallback(
    async (path: string) => {
      if (isDirty() && !window.confirm("You have unsaved changes. Discard them?")) return;
      setLoading(true);
      setShowOpenMenu(false);
      try {
        const result = await api.openFile(path);
        setDocument(result.document);
        setDiagnostics(result.diagnostics);
        setCurrentFilePath(result.path);
        markSaved();
      } catch (err) {
        console.error("Failed to open file:", err);
      } finally {
        setLoading(false);
      }
    },
    [setDocument, setDiagnostics, setCurrentFilePath, markSaved, isDirty],
  );

  const handleImport = useCallback(
    async (e: React.ChangeEvent<HTMLInputElement>) => {
      const file = e.target.files?.[0];
      if (!file) return;
      if (isDirty() && !window.confirm("You have unsaved changes. Discard them?")) {
        e.target.value = "";
        return;
      }
      const text = await file.text();
      try {
        const result = await api.parseSource(text);
        setDocument(result.document);
        setDiagnostics(result.diagnostics);
        setCurrentFilePath(null);
      } catch (err) {
        console.error("Import failed:", err);
      }
      // Reset input so the same file can be re-imported
      e.target.value = "";
    },
    [setDocument, setDiagnostics, setCurrentFilePath, isDirty],
  );

  const handleValidate = useCallback(async () => {
    if (!document) return;
    try {
      const result = await api.validate(document);
      setDiagnostics(result.diagnostics, result.warnings);
      const errorCount = (result.diagnostics ?? []).length;
      const warnCount = (result.warnings ?? []).length;
      if (errorCount === 0 && warnCount === 0) {
        addToast("No issues found", "success");
      } else {
        addToast(`${errorCount} error${errorCount !== 1 ? "s" : ""}, ${warnCount} warning${warnCount !== 1 ? "s" : ""}`, "error");
      }
    } catch (err) {
      console.error("Validation failed:", err);
      addToast("Validation failed", "error");
    }
  }, [document, setDiagnostics, addToast]);

  const handleSave = useCallback(async () => {
    if (!document) return;
    if (currentFilePath) {
      // Save to existing file
      try {
        await api.saveFile(currentFilePath, document);
        markSaved();
        addToast("Saved successfully", "success");
        // Refresh file list
        api.listFiles().then(setFiles).catch(() => {});
      } catch (err) {
        console.error("Save failed:", err);
        addToast("Save failed", "error");
      }
    } else {
      // Show save dialog for new file
      const name = document.workflows?.[0]?.name || "workflow";
      setSaveFileName(`${name}.iter`);
      setShowSaveDialog(true);
    }
  }, [document, currentFilePath, markSaved, addToast]);

  const handleSaveAs = useCallback(async () => {
    if (!document || !saveFileName) return;
    const fileName = saveFileName.endsWith(".iter") ? saveFileName : `${saveFileName}.iter`;
    try {
      const result = await api.saveFile(fileName, document);
      setCurrentFilePath(result.path);
      markSaved();
      addToast("Saved successfully", "success");
      setShowSaveDialog(false);
      // Refresh file list
      api.listFiles().then(setFiles).catch(() => {});
    } catch (err) {
      console.error("Save failed:", err);
    }
  }, [document, saveFileName, setCurrentFilePath]);

  // Keyboard shortcuts (must be after handleSave is defined)
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === "z" && !e.shiftKey) {
        e.preventDefault();
        undo();
      } else if ((e.ctrlKey || e.metaKey) && (e.key === "y" || (e.key === "z" && e.shiftKey))) {
        e.preventDefault();
        redo();
      } else if ((e.ctrlKey || e.metaKey) && e.key === "s") {
        e.preventDefault();
        handleSave();
      } else if (e.key === "?" && !(e.target as HTMLElement).matches("input, textarea, select")) {
        e.preventDefault();
        setShowShortcuts(true);
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [undo, redo, handleSave]);

  const handleDownload = useCallback(async () => {
    if (!document) return;
    try {
      const source = await api.unparse(document);
      const blob = new Blob([source], { type: "text/plain" });
      const url = URL.createObjectURL(blob);
      const a = window.document.createElement("a");
      a.href = url;
      const name = document.workflows?.[0]?.name || "workflow";
      a.download = `${name}.iter`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      console.error("Download failed:", err);
    }
  }, [document]);

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

  const workflows = document?.workflows ?? [];

  return (
    <div className="flex items-center gap-2 px-4 h-full text-sm">
      <span className="font-bold tracking-wide">ITERION</span>
      <div className="h-4 w-px bg-gray-600" />

      {/* File operations */}
      <button className="bg-green-700 hover:bg-green-600 px-2.5 py-1 rounded" onClick={handleNew}>
        New
      </button>

      <div className="relative" ref={openMenuRef}>
        <button
          className="bg-gray-700 hover:bg-gray-600 px-2.5 py-1 rounded"
          onClick={() => { setShowOpenMenu(!showOpenMenu); api.listFiles().then(setFiles).catch(() => {}); }}
        >
          Open
        </button>
        {showOpenMenu && (
          <div className="absolute top-full left-0 mt-1 bg-gray-800 border border-gray-600 rounded shadow-xl z-50 min-w-[200px] max-h-[300px] overflow-y-auto">
            {files.length > 0 && (
              <>
                <div className="px-3 py-1.5 text-[10px] text-gray-500 uppercase tracking-wider">Files</div>
                {files.map((f) => (
                  <button
                    key={f.name}
                    className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs truncate"
                    onClick={() => handleOpenFile(f.name)}
                  >
                    {f.name}
                  </button>
                ))}
              </>
            )}
            {files.length > 0 && examples.length > 0 && <div className="border-t border-gray-700" />}
            {examples.length > 0 && (
              <>
                <div className="px-3 py-1.5 text-[10px] text-gray-500 uppercase tracking-wider">Examples</div>
                {examples.map((name) => (
                  <button
                    key={name}
                    className="w-full text-left px-3 py-1.5 hover:bg-gray-700 text-xs truncate"
                    onClick={() => { loadExample(name); setShowOpenMenu(false); }}
                  >
                    {name}
                  </button>
                ))}
              </>
            )}
            {files.length === 0 && examples.length === 0 && (
              <div className="px-3 py-2 text-xs text-gray-500">No files found</div>
            )}
          </div>
        )}
      </div>

      <button
        className="bg-gray-700 hover:bg-gray-600 px-2.5 py-1 rounded"
        onClick={() => fileInputRef.current?.click()}
      >
        Import
      </button>
      <input ref={fileInputRef} type="file" accept=".iter" className="hidden" onChange={handleImport} />

      <div className="h-4 w-px bg-gray-600" />

      {/* Save operations */}
      <button
        className="bg-indigo-600 hover:bg-indigo-700 px-2.5 py-1 rounded disabled:opacity-50"
        onClick={handleSave}
        disabled={!document}
        title={currentFilePath ? `Save to ${currentFilePath}` : "Save as..."}
      >
        Save{currentFilePath ? "" : " As"}
      </button>

      <button
        className="bg-gray-700 hover:bg-gray-600 px-2.5 py-1 rounded disabled:opacity-50"
        onClick={handleDownload}
        disabled={!document}
        title="Download .iter file"
      >
        Download
      </button>

      <button
        className="bg-gray-700 hover:bg-gray-600 px-2.5 py-1 rounded disabled:opacity-50"
        onClick={handleCopySource}
        disabled={!document}
        title="Copy source to clipboard"
      >
        Copy
      </button>

      <div className="h-4 w-px bg-gray-600" />

      {/* Undo/Redo */}
      <button
        className="bg-gray-700 hover:bg-gray-600 px-2 py-1 rounded disabled:opacity-30"
        onClick={undo}
        disabled={!canUndo()}
        title="Undo (Ctrl+Z)"
      >
        &#x21A9;
      </button>
      <button
        className="bg-gray-700 hover:bg-gray-600 px-2 py-1 rounded disabled:opacity-30"
        onClick={redo}
        disabled={!canRedo()}
        title="Redo (Ctrl+Y)"
      >
        &#x21AA;
      </button>

      <div className="h-4 w-px bg-gray-600" />

      {/* Validate */}
      <button
        className="bg-blue-600 hover:bg-blue-700 px-2.5 py-1 rounded disabled:opacity-50"
        onClick={handleValidate}
        disabled={!document}
      >
        Validate
      </button>

      {/* Source view toggle */}
      <button
        className={`px-2.5 py-1 rounded ${
          sourceViewOpen ? "bg-purple-600 hover:bg-purple-700" : "bg-gray-700 hover:bg-gray-600"
        }`}
        onClick={toggleSourceView}
      >
        Source
      </button>

      {/* Diagnostics panel toggle */}
      <button
        className={`px-2.5 py-1 rounded ${
          diagnosticsPanelOpen ? "bg-orange-600 hover:bg-orange-700" : "bg-gray-700 hover:bg-gray-600"
        }`}
        onClick={toggleDiagnosticsPanel}
      >
        Diagnostics
      </button>

      <button
        className="bg-gray-700 hover:bg-gray-600 px-2 py-1 rounded text-xs"
        onClick={() => setShowShortcuts(true)}
        title="Keyboard shortcuts (?)"
      >
        ?
      </button>

      <div className="h-4 w-px bg-gray-600" />

      {/* Workflow selector */}
      {workflows.length > 0 && (
        <>
          <select
            className="bg-gray-800 border border-gray-600 rounded px-2 py-1 text-xs"
            value={activeWorkflowName ?? workflows[0]?.name ?? ""}
            onChange={(e) => setActiveWorkflowName(e.target.value)}
          >
            {workflows.map((w) => (
              <option key={w.name} value={w.name}>
                {w.name}
              </option>
            ))}
          </select>
          <button
            className="bg-green-800 hover:bg-green-700 px-1.5 py-1 rounded text-xs"
            onClick={handleAddWorkflow}
            title="Add workflow"
          >
            +
          </button>
          {workflows.length > 1 && (
            <button
              className="bg-red-900 hover:bg-red-800 px-1.5 py-1 rounded text-xs"
              onClick={() => setConfirmRemoveWorkflow(true)}
              title="Remove current workflow"
            >
              -
            </button>
          )}
        </>
      )}

      {/* File path indicator */}
      {currentFilePath && (
        <span className="text-[10px] text-gray-500 ml-2 truncate max-w-[200px]" title={currentFilePath}>
          {isDirty() && <span className="text-yellow-400">* </span>}
          {currentFilePath}
        </span>
      )}
      {!currentFilePath && document && isDirty() && (
        <span className="text-[10px] text-yellow-400 ml-2">* unsaved</span>
      )}

      {loading && <span className="text-xs text-gray-400">Loading...</span>}

      <ConfirmDialog
        open={confirmRemoveWorkflow}
        title="Remove Workflow"
        message={`Remove workflow "${activeWorkflowName}"? This cannot be undone.`}
        confirmLabel="Remove"
        confirmVariant="danger"
        onConfirm={() => { handleRemoveWorkflow(); setConfirmRemoveWorkflow(false); }}
        onCancel={() => setConfirmRemoveWorkflow(false)}
      />

      {/* Save dialog */}
      {showSaveDialog && (
        <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50">
          <div className="bg-gray-800 border border-gray-600 rounded-lg p-4 min-w-[300px]">
            <h3 className="text-sm font-bold mb-3">Save As</h3>
            <input
              className="w-full bg-gray-900 border border-gray-600 rounded px-3 py-2 text-sm mb-3 focus:border-blue-500 focus:outline-none"
              value={saveFileName}
              onChange={(e) => setSaveFileName(e.target.value)}
              placeholder="filename.iter"
              autoFocus
              onKeyDown={(e) => { if (e.key === "Enter") handleSaveAs(); if (e.key === "Escape") setShowSaveDialog(false); }}
            />
            <div className="flex justify-end gap-2">
              <button
                className="bg-gray-700 hover:bg-gray-600 px-3 py-1.5 rounded text-xs"
                onClick={() => setShowSaveDialog(false)}
              >
                Cancel
              </button>
              <button
                className="bg-indigo-600 hover:bg-indigo-700 px-3 py-1.5 rounded text-xs"
                onClick={handleSaveAs}
              >
                Save
              </button>
            </div>
          </div>
        </div>
      )}

      <ShortcutsHelp open={showShortcuts} onClose={() => setShowShortcuts(false)} />
    </div>
  );
}
