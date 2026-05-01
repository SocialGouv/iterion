import { useCallback, useEffect, useRef, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useRecentsStore } from "@/store/recents";
import { createEmptyDocument } from "@/lib/defaults";
import * as api from "@/api/client";
import ConfirmDialog from "../shared/ConfirmDialog";
import ShortcutsHelp from "../shared/ShortcutsHelp";
import FilePicker from "../FilePicker/FilePicker";
import {
  Button,
  IconButton,
  Select,
  Dialog,
  Input,
} from "@/components/ui";
import ThemeToggle from "@/components/ui/ThemeToggle";
import ToolbarGroup from "./ToolbarGroup";
import {
  FilePlusIcon,
  Pencil2Icon,
  DownloadIcon,
  UploadIcon,
  CopyIcon,
  ResetIcon,
  ChevronRightIcon,
  CheckCircledIcon,
  EyeOpenIcon,
  ExclamationTriangleIcon,
  QuestionMarkCircledIcon,
  PlusIcon,
  MinusIcon,
  DotsHorizontalIcon,
  ListBulletIcon,
  PlayIcon,
} from "@radix-ui/react-icons";
import { useLocation } from "wouter";

export default function Toolbar() {
  const [, setLocation] = useLocation();
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
  const pushRecent = useRecentsStore((s) => s.pushRecent);

  const filePickerOpen = useUIStore((s) => s.filePickerOpen);
  const setFilePickerOpen = useUIStore((s) => s.setFilePickerOpen);
  const [loading, setLoading] = useState(false);
  const [showSaveDialog, setShowSaveDialog] = useState(false);
  const [saveFileName, setSaveFileName] = useState("");
  const [overflowOpen, setOverflowOpen] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const overflowRef = useRef<HTMLDivElement>(null);
  const [confirmRemoveWorkflow, setConfirmRemoveWorkflow] = useState(false);
  const [showShortcuts, setShowShortcuts] = useState(false);

  // Close overflow menu on outside click
  useEffect(() => {
    if (!overflowOpen) return;
    const handler = (e: MouseEvent) => {
      if (overflowRef.current && !overflowRef.current.contains(e.target as Node)) {
        setOverflowOpen(false);
      }
    };
    window.document.addEventListener("mousedown", handler);
    return () => window.document.removeEventListener("mousedown", handler);
  }, [overflowOpen]);

  const handleNew = useCallback(() => {
    if (isDirty() && !window.confirm("You have unsaved changes. Discard them?")) return;
    setDocument(createEmptyDocument());
    setDiagnostics([], []);
    setCurrentFilePath(null);
    markSaved();
  }, [setDocument, setDiagnostics, setCurrentFilePath, markSaved, isDirty]);

  const handlePickFile = useCallback(
    async (kind: "file" | "example", path: string) => {
      if (isDirty() && !window.confirm("You have unsaved changes. Discard them?")) return;
      setLoading(true);
      try {
        if (kind === "file") {
          const result = await api.openFile(path);
          setDocument(result.document);
          setDiagnostics(result.diagnostics);
          setCurrentFilePath(result.path);
          pushRecent(result.path);
          markSaved();
        } else {
          const result = await api.loadExample(path);
          setDocument(result.document);
          setDiagnostics(result.diagnostics);
          // Examples live at <WorkDir>/examples/<name>, so the same
          // relative path is reachable via the file API. Setting it
          // here lets the user Save edits back to the example file
          // and — more importantly — enables the Launch run button,
          // which would otherwise be disabled with "Save the workflow
          // first" on every example.
          setCurrentFilePath(`examples/${path}`);
          markSaved();
        }
      } catch (err) {
        console.error("Open failed:", err);
        addToast("Open failed", "error");
      } finally {
        setLoading(false);
      }
    },
    [setDocument, setDiagnostics, setCurrentFilePath, markSaved, isDirty, pushRecent, addToast],
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
      e.target.value = "";
    },
    [setDocument, setDiagnostics, setCurrentFilePath, isDirty],
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
      try {
        await api.saveFile(currentFilePath, document);
        markSaved();
        addToast("Saved successfully", "success");
        pushRecent(currentFilePath);
      } catch (err) {
        console.error("Save failed:", err);
        addToast("Save failed", "error");
      }
    } else {
      const name = document.workflows?.[0]?.name || "workflow";
      setSaveFileName(`${name}.iter`);
      setShowSaveDialog(true);
    }
  }, [document, currentFilePath, markSaved, addToast, pushRecent]);

  const handleSaveAs = useCallback(async () => {
    if (!document || !saveFileName) return;
    const fileName = saveFileName.endsWith(".iter") ? saveFileName : `${saveFileName}.iter`;
    try {
      const result = await api.saveFile(fileName, document);
      setCurrentFilePath(result.path);
      markSaved();
      pushRecent(result.path);
      addToast("Saved successfully", "success");
      setShowSaveDialog(false);
    } catch (err) {
      console.error("Save failed:", err);
      addToast("Save failed", "error");
    }
  }, [document, saveFileName, setCurrentFilePath, markSaved, pushRecent, addToast]);

  // Keyboard shortcuts
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
      } else if ((e.ctrlKey || e.metaKey) && e.key === "o") {
        e.preventDefault();
        setFilePickerOpen(true);
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
    <div className="flex items-center gap-1 px-3 h-full text-sm">
      <span className="font-bold tracking-wide mr-2">ITERION</span>

      {/* File ops */}
      <ToolbarGroup>
        <IconButton label="New (Ctrl+N)" tooltip="New workflow" onClick={handleNew} size="sm">
          <FilePlusIcon />
        </IconButton>
        <IconButton
          label="Open (Ctrl+O)"
          tooltip="Open file"
          size="sm"
          onClick={() => setFilePickerOpen(true)}
        >
          <Pencil2Icon />
        </IconButton>
        <Button
          variant="primary"
          size="sm"
          leadingIcon={<DownloadIcon />}
          onClick={handleSave}
          disabled={!document}
          title={currentFilePath ? `Save to ${currentFilePath}` : "Save as..."}
        >
          {currentFilePath ? "Save" : "Save As"}
        </Button>
        <div className="relative" ref={overflowRef}>
          <IconButton
            label="More file actions"
            tooltip="More"
            size="sm"
            onClick={() => setOverflowOpen((v) => !v)}
            active={overflowOpen}
          >
            <DotsHorizontalIcon />
          </IconButton>
          {overflowOpen && (
            <div className="absolute top-full left-0 mt-1 z-50 min-w-[180px] rounded-md border border-border-default bg-surface-1 shadow-xl py-1">
              <OverflowItem
                icon={<UploadIcon />}
                label="Import .iter file"
                onClick={() => {
                  setOverflowOpen(false);
                  fileInputRef.current?.click();
                }}
              />
              <OverflowItem
                icon={<DownloadIcon />}
                label="Download as .iter"
                onClick={() => {
                  setOverflowOpen(false);
                  handleDownload();
                }}
                disabled={!document}
              />
              <OverflowItem
                icon={<CopyIcon />}
                label="Copy source to clipboard"
                onClick={() => {
                  setOverflowOpen(false);
                  handleCopySource();
                }}
                disabled={!document}
              />
            </div>
          )}
        </div>
      </ToolbarGroup>

      {/* Edit */}
      <ToolbarGroup>
        <IconButton
          label="Undo (Ctrl+Z)"
          tooltip="Undo"
          size="sm"
          onClick={undo}
          disabled={!canUndo()}
        >
          <ResetIcon />
        </IconButton>
        <IconButton
          label="Redo (Ctrl+Y)"
          tooltip="Redo"
          size="sm"
          onClick={redo}
          disabled={!canRedo()}
        >
          <ChevronRightIcon />
        </IconButton>
      </ToolbarGroup>

      {/* View */}
      <ToolbarGroup>
        <IconButton
          label="Validate"
          tooltip="Validate workflow"
          size="sm"
          onClick={handleValidate}
          disabled={!document}
        >
          <CheckCircledIcon />
        </IconButton>
        <IconButton
          label="Toggle source view"
          tooltip="Source"
          size="sm"
          onClick={toggleSourceView}
          active={sourceViewOpen}
        >
          <EyeOpenIcon />
        </IconButton>
        <IconButton
          label="Toggle diagnostics panel"
          tooltip="Diagnostics"
          size="sm"
          onClick={toggleDiagnosticsPanel}
          active={diagnosticsPanelOpen}
        >
          <ExclamationTriangleIcon />
        </IconButton>
        <ThemeToggle />
      </ToolbarGroup>

      {/* Workflow */}
      {workflows.length > 0 && (
        <ToolbarGroup>
          <Select
            value={activeWorkflowName ?? workflows[0]?.name ?? ""}
            onChange={(e) => setActiveWorkflowName(e.target.value)}
            size="sm"
          >
            {workflows.map((w) => (
              <option key={w.name} value={w.name}>
                {w.name}
              </option>
            ))}
          </Select>
          <IconButton label="Add workflow" tooltip="Add workflow" size="sm" onClick={handleAddWorkflow}>
            <PlusIcon />
          </IconButton>
          {workflows.length > 1 && (
            <IconButton
              label="Remove current workflow"
              tooltip="Remove workflow"
              variant="danger"
              size="sm"
              onClick={() => setConfirmRemoveWorkflow(true)}
            >
              <MinusIcon />
            </IconButton>
          )}
        </ToolbarGroup>
      )}

      <input ref={fileInputRef} type="file" accept=".iter" className="hidden" onChange={handleImport} />

      {/* Right-aligned: file path + help + run console */}
      <div className="ml-auto flex items-center gap-2">
        {currentFilePath && (
          <span className="text-[10px] text-fg-subtle truncate max-w-[280px]" title={currentFilePath}>
            {isDirty() && <span className="text-warning">* </span>}
            {currentFilePath}
          </span>
        )}
        {!currentFilePath && document && isDirty() && (
          <span className="text-[10px] text-warning">* unsaved</span>
        )}
        {loading && <span className="text-xs text-fg-subtle">Loading...</span>}
        <IconButton
          label="Launch run"
          tooltip={
            currentFilePath
              ? `Launch ${currentFilePath}`
              : "Save the workflow first to launch a run"
          }
          size="sm"
          onClick={() =>
            setLocation(`/runs/new?file=${encodeURIComponent(currentFilePath ?? "")}`)
          }
          disabled={!currentFilePath}
        >
          <PlayIcon />
        </IconButton>
        <IconButton
          label="Run console"
          tooltip="Open run console"
          size="sm"
          onClick={() => setLocation("/runs")}
        >
          <ListBulletIcon />
        </IconButton>
        <IconButton
          label="Keyboard shortcuts (?)"
          tooltip="Shortcuts"
          size="sm"
          onClick={() => setShowShortcuts(true)}
        >
          <QuestionMarkCircledIcon />
        </IconButton>
      </div>

      <FilePicker
        open={filePickerOpen}
        onOpenChange={setFilePickerOpen}
        onPick={handlePickFile}
      />

      <ConfirmDialog
        open={confirmRemoveWorkflow}
        title="Remove Workflow"
        message={`Remove workflow "${activeWorkflowName}"? This cannot be undone.`}
        confirmLabel="Remove"
        confirmVariant="danger"
        onConfirm={() => {
          handleRemoveWorkflow();
          setConfirmRemoveWorkflow(false);
        }}
        onCancel={() => setConfirmRemoveWorkflow(false)}
      />

      {/* Save dialog */}
      <Dialog
        open={showSaveDialog}
        onOpenChange={setShowSaveDialog}
        title="Save As"
        widthClass="max-w-sm"
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setShowSaveDialog(false)}>
              Cancel
            </Button>
            <Button variant="primary" size="sm" onClick={handleSaveAs}>
              Save
            </Button>
          </>
        }
      >
        <Input
          autoFocus
          value={saveFileName}
          onChange={(e) => setSaveFileName(e.target.value)}
          placeholder="filename.iter"
          size="md"
          onKeyDown={(e) => {
            if (e.key === "Enter") handleSaveAs();
            if (e.key === "Escape") setShowSaveDialog(false);
          }}
        />
      </Dialog>

      <ShortcutsHelp open={showShortcuts} onClose={() => setShowShortcuts(false)} />
    </div>
  );
}

function OverflowItem({
  icon,
  label,
  onClick,
  disabled,
}: {
  icon: React.ReactNode;
  label: string;
  onClick: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="w-full flex items-center gap-2 px-3 py-1.5 text-xs text-fg-default hover:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed"
    >
      {icon}
      <span>{label}</span>
    </button>
  );
}
