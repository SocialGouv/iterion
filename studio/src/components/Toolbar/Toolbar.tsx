import { useCallback, useEffect, useRef, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useRecentsStore } from "@/store/recents";
import { useBackendDetectStore } from "@/store/backendDetect";
import { createEmptyDocument } from "@/lib/defaults";
import * as api from "@/api/client";
import ConfirmDialog from "../shared/ConfirmDialog";
import { useConfirm } from "@/hooks/useConfirm";
import { DISCARD_CHANGES_PROMPT } from "@/lib/copy";
import ShortcutsHelp from "../shared/ShortcutsHelp";
import FilePicker from "../FilePicker/FilePicker";
import {
  Button,
  IconButton,
  Select,
  Dialog,
  Input,
  Popover,
  PopoverClose,
} from "@/components/ui";
import ToolbarGroup from "./ToolbarGroup";
import {
  FilePlusIcon,
  Pencil2Icon,
  DownloadIcon,
  UploadIcon,
  CopyIcon,
  ResetIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  CheckCircledIcon,
  EyeOpenIcon,
  ExclamationTriangleIcon,
  QuestionMarkCircledIcon,
  PlusIcon,
  MinusIcon,
  ArrowDownIcon,
  ArrowRightIcon,
  StackIcon,
  FrameIcon,
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
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
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
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const toggleLayoutDirection = useUIStore((s) => s.toggleLayoutDirection);
  const canvasActions = useUIStore((s) => s.canvasActions);
  const addToast = useUIStore((s) => s.addToast);
  const pushRecent = useRecentsStore((s) => s.pushRecent);
  const removeRecent = useRecentsStore((s) => s.removeRecent);

  const filePickerOpen = useUIStore((s) => s.filePickerOpen);
  const setFilePickerOpen = useUIStore((s) => s.setFilePickerOpen);
  const hasResolvedBackend = useBackendDetectStore((s) => !!s.report?.resolved_default);
  const [loading, setLoading] = useState(false);
  const [showSaveDialog, setShowSaveDialog] = useState(false);
  const [saveFileName, setSaveFileName] = useState("");
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [confirmRemoveWorkflow, setConfirmRemoveWorkflow] = useState(false);
  const [showShortcuts, setShowShortcuts] = useState(false);
  const { confirm, dialog: confirmDialog } = useConfirm();

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
  }, [setDocument, setDiagnostics, setCurrentFilePath, setCurrentSource, markSaved, confirmDiscard]);

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
          // loadExample doesn't return raw .iter source (it returns the
          // parsed document only). Cloud-mode resume after launching
          // an example would need a re-open to populate currentSource.
          setCurrentSource(null);
          markSaved();
        }
      } catch (err) {
        console.error("Open failed:", err);
        // Auto-clean stale recents: a 404 from /files/open means the
        // file underlying this recent entry no longer exists (deleted,
        // moved, or the workspace was switched to a project that
        // doesn't contain it). Pruning here means the next time the
        // picker opens, the dead row is gone — instead of the user
        // having to manually click the trash icon on every stale row.
        const message = (err as Error).message ?? "";
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
    [setDocument, setDiagnostics, setCurrentFilePath, markSaved, confirmDiscard, pushRecent, removeRecent, addToast],
  );

  const handleImport = useCallback(
    async (e: React.ChangeEvent<HTMLInputElement>) => {
      const file = e.target.files?.[0];
      if (!file) return;
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
      }
      e.target.value = "";
    },
    [setDocument, setDiagnostics, setCurrentFilePath, setCurrentSource, confirmDiscard],
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
  }, [document, currentFilePath, setCurrentSource, markSaved, addToast, pushRecent]);

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
    const fileName =
      saveFileName.endsWith(".iter") || saveFileName.endsWith(".bot")
        ? saveFileName
        : `${saveFileName}.bot`;
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
  }, [document, saveFileName, setCurrentFilePath, setCurrentSource, markSaved, pushRecent, addToast]);

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
      a.download = `${name}.bot`;
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
    <div className="flex items-center gap-1 px-4 h-10 text-sm bg-surface-1 border-b border-border-default">
      {/* File menu (VSCode-style unified dropdown) + inline primary Save */}
      <ToolbarGroup>
        <Popover
          side="bottom"
          align="start"
          contentClassName="p-1 min-w-[220px]"
          trigger={
            <Button
              variant="secondary"
              size="sm"
              trailingIcon={<ChevronDownIcon />}
              aria-label="File menu"
              title="File"
            >
              File
            </Button>
          }
        >
          <div className="flex flex-col">
            <FileMenuItem
              icon={<FilePlusIcon />}
              label="New"
              shortcut="Ctrl+N"
              onSelect={handleNew}
            />
            <FileMenuItem
              icon={<Pencil2Icon />}
              label="Open…"
              shortcut="Ctrl+O"
              onSelect={() => setFilePickerOpen(true)}
            />
            <MenuSeparator />
            <FileMenuItem
              icon={<DownloadIcon />}
              label="Save"
              shortcut="Ctrl+S"
              onSelect={handleSave}
              disabled={!document}
            />
            <FileMenuItem
              icon={<DownloadIcon />}
              label="Save As…"
              onSelect={handleSaveAsRequest}
              disabled={!document}
            />
            <MenuSeparator />
            <FileMenuItem
              icon={<UploadIcon />}
              label="Import workflow file"
              onSelect={() => fileInputRef.current?.click()}
            />
            <FileMenuItem
              icon={<DownloadIcon />}
              label="Download as .bot"
              onSelect={handleDownload}
              disabled={!document}
            />
            <FileMenuItem
              icon={<CopyIcon />}
              label="Copy source to clipboard"
              onSelect={handleCopySource}
              disabled={!document}
            />
          </div>
        </Popover>
        <Button
          variant="primary"
          size="sm"
          leadingIcon={<DownloadIcon />}
          onClick={handleSave}
          disabled={!document}
          title={currentFilePath ? `Save to ${currentFilePath} (Ctrl+S)` : "Save as… (Ctrl+S)"}
        >
          {currentFilePath ? "Save" : "Save As"}
        </Button>
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
        <IconButton
          label={
            layoutDirection === "DOWN"
              ? "Switch to horizontal layout (left→right)"
              : "Switch to vertical layout (top→bottom)"
          }
          tooltip="Layout direction"
          size="sm"
          onClick={toggleLayoutDirection}
        >
          {layoutDirection === "DOWN" ? <ArrowRightIcon /> : <ArrowDownIcon />}
        </IconButton>
        <IconButton
          label="Arrange (auto-layout)"
          tooltip="Arrange"
          size="sm"
          onClick={() => canvasActions.arrange?.()}
          disabled={!canvasActions.arrange}
        >
          <StackIcon />
        </IconButton>
        <IconButton
          label="Fit view"
          tooltip="Fit view"
          size="sm"
          onClick={() => canvasActions.fitView?.()}
          disabled={!canvasActions.fitView}
        >
          <FrameIcon />
        </IconButton>
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

      <input ref={fileInputRef} type="file" accept=".iter,.bot" className="hidden" onChange={handleImport} />

      {/* Right-aligned: file status → Launch run; then navigation icons */}
      <div className="ml-auto flex items-center gap-3">
        {loading && <span className="text-xs text-fg-subtle">Loading...</span>}
        <div className="flex items-center gap-2">
          <FileStatusBadge
            currentFilePath={currentFilePath}
            hasDocument={!!document}
            isDirty={isDirty()}
          />
          <Button
            variant="primary"
            size="sm"
            leadingIcon={<PlayIcon />}
            onClick={() =>
              setLocation(`/runs/new?file=${encodeURIComponent(currentFilePath ?? "")}`)
            }
            disabled={!currentFilePath || !hasResolvedBackend}
            title={
              !currentFilePath
                ? "Save the workflow first to launch a run"
                : !hasResolvedBackend
                ? "No LLM credentials detected — click the status pill on the left to configure."
                : `Launch ${currentFilePath}`
            }
          >
            Run
          </Button>
        </div>
        <div className="flex items-center gap-1 pl-2 border-l border-border-default">
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
          placeholder="filename.bot"
          size="md"
          onKeyDown={(e) => {
            if (e.key === "Enter") handleSaveAs();
            if (e.key === "Escape") setShowSaveDialog(false);
          }}
        />
      </Dialog>

      <ShortcutsHelp open={showShortcuts} onClose={() => setShowShortcuts(false)} />
      {confirmDialog}
    </div>
  );
}

function FileMenuItem({
  icon,
  label,
  shortcut,
  onSelect,
  disabled,
}: {
  icon: React.ReactNode;
  label: string;
  shortcut?: string;
  onSelect: () => void;
  disabled?: boolean;
}) {
  // PopoverClose closes the popover on activation; wrapping it around
  // the menu button means each choice dismisses the menu automatically
  // — no manual open-state plumbing on the parent.
  const button = (
    <button
      type="button"
      onClick={onSelect}
      disabled={disabled}
      className="w-full flex items-center gap-2 rounded px-2 py-1.5 text-left text-xs text-fg-default hover:bg-surface-2 focus:outline-none focus:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed"
    >
      <span className="text-fg-muted">{icon}</span>
      <span className="flex-1">{label}</span>
      {shortcut && (
        <span className="text-[10px] text-fg-subtle font-mono">{shortcut}</span>
      )}
    </button>
  );
  // Disabled items don't dismiss the popover — let users keep
  // exploring other menu entries without the menu collapsing.
  return disabled ? button : <PopoverClose asChild>{button}</PopoverClose>;
}

function MenuSeparator() {
  return <div className="my-1 h-px bg-border-default" aria-hidden />;
}

function FileStatusBadge({
  currentFilePath,
  hasDocument,
  isDirty,
}: {
  currentFilePath: string | null;
  hasDocument: boolean;
  isDirty: boolean;
}) {
  // Render nothing when the studio has no document at all — the toolbar
  // is in its empty state and the right-side group is just navigation.
  if (!hasDocument) return null;
  // Split the path into parent + basename so the breadcrumb can show
  // "examples/feature_dev › main.bot" with the parent slightly
  // muted. Parents longer than ~20 chars are truncated from the LEFT
  // so the trailing directory (most informative) stays visible.
  let parent = "";
  let basename = "Untitled";
  if (currentFilePath) {
    const idx = currentFilePath.lastIndexOf("/");
    if (idx >= 0) {
      parent = currentFilePath.slice(0, idx);
      basename = currentFilePath.slice(idx + 1) || currentFilePath;
    } else {
      basename = currentFilePath;
    }
  }
  const displayParent =
    parent.length > 24 ? "…" + parent.slice(parent.length - 22) : parent;
  const subtitle = !currentFilePath
    ? "Unsaved"
    : isDirty
    ? "Modified"
    : null;
  return (
    <div
      className="flex items-center gap-1.5 max-w-[360px] border border-border-default rounded px-2 py-0.5 bg-surface-1"
      title={currentFilePath ?? "Untitled — save the workflow to give it a path"}
    >
      {isDirty && (
        <span
          aria-hidden
          className="inline-block w-1.5 h-1.5 rounded-full bg-warning shrink-0"
        />
      )}
      {displayParent && (
        <span className="truncate text-[10px] text-fg-subtle font-mono shrink min-w-0">
          {displayParent}
        </span>
      )}
      {displayParent && (
        <span aria-hidden className="text-fg-subtle text-[10px] shrink-0">
          ›
        </span>
      )}
      <span className="truncate text-xs text-fg-default font-medium">
        {basename}
      </span>
      {subtitle && (
        <span className="text-[10px] text-fg-subtle shrink-0">{subtitle}</span>
      )}
    </div>
  );
}
