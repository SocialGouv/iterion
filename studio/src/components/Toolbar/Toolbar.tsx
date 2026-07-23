import { useEffect, useRef, useState } from "react";
import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useRecentsStore } from "@/store/recents";
import { useBackendDetectStore } from "@/store/backendDetect";
import * as api from "@/api/client";
import ConfirmDialog from "../shared/ConfirmDialog";
import { useConfirm } from "@/hooks/useConfirm";
import { Spinner } from "@/components/ui/Spinner";
import ShortcutsHelp from "../shared/ShortcutsHelp";
import FilePicker from "../FilePicker/FilePicker";
import {
  Button,
  IconButton,
  Select,
  Dialog,
  Input,
  DropdownMenu,
  DropdownMenuItem,
  DropdownMenuSub,
  DropdownMenuSeparator,
} from "@/components/ui";
import ToolbarGroup from "./ToolbarGroup";
import { useDocumentFileOps } from "./useDocumentFileOps";
import BundleFilesDrawer from "@/components/Editor/BundleFilesDrawer";
import { forkBotSource } from "@/api/botSources";
import { botSourceEditorPath } from "@/api/client";
import { useAuth } from "@/auth/AuthContext";
import { useBotsStore } from "@/store/bots";
import { usePromptText } from "@/hooks/usePromptText";
import { useTabsStore } from "@/store/tabs";
import { toastError } from "@/lib/errorHints";
import {
  FilePlusIcon,
  DownloadIcon,
  UploadIcon,
  CopyIcon,
  ResetIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  ClockIcon,
  RocketIcon,
  TrashIcon,
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
  const document = useDocumentStore((s) => s.document);
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const undo = useDocumentStore((s) => s.undo);
  const redo = useDocumentStore((s) => s.redo);
  const canUndo = useDocumentStore((s) => s.canUndo);
  const canRedo = useDocumentStore((s) => s.canRedo);
  const isDirty = useDocumentStore((s) => s.isDirty);
  const addToast = useUIStore((s) => s.addToast);
  const sourceViewOpen = useUIStore((s) => s.sourceViewOpen);
  const toggleSourceView = useUIStore((s) => s.toggleSourceView);
  const diagnosticsPanelOpen = useUIStore((s) => s.diagnosticsPanelOpen);
  const toggleDiagnosticsPanel = useUIStore((s) => s.toggleDiagnosticsPanel);
  const activeWorkflowName = useUIStore((s) => s.activeWorkflowName);
  const setActiveWorkflowName = useUIStore((s) => s.setActiveWorkflowName);
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const toggleLayoutDirection = useUIStore((s) => s.toggleLayoutDirection);
  const canvasActions = useUIStore((s) => s.canvasActions);

  // FilePicker modal stays the searchable cross-source picker for the
  // blank-canvas tile and the Cmd+K command palette; the File menu below
  // no longer opens it (Open… is now the native OS picker).
  const filePickerOpen = useUIStore((s) => s.filePickerOpen);
  const setFilePickerOpen = useUIStore((s) => s.setFilePickerOpen);
  const recents = useRecentsStore((s) => s.recents);
  const clearRecents = useRecentsStore((s) => s.clearRecents);
  const hasResolvedBackend = useBackendDetectStore((s) => !!s.report?.resolved_default);
  // `report != null` once the host probe has returned (success or fail) —
  // gates the missing-credential nudge so it doesn't flash during the boot probe.
  const backendProbed = useBackendDetectStore((s) => s.report != null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [showShortcuts, setShowShortcuts] = useState(false);
  // Examples list for the File menu submenu — fetched lazily the first
  // time the menu opens (same source as FilePicker), then cached.
  const [examples, setExamples] = useState<string[] | null>(null);
  const [examplesLoading, setExamplesLoading] = useState(false);
  const { confirm, dialog: confirmDialog } = useConfirm();

  // Open… and Ctrl+O both trigger the native OS file picker (merged with
  // the former "Import" action). Recents/Examples below keep binding the
  // workspace path so Save/Run stay enabled.
  const openNative = () => fileInputRef.current?.click();

  const loadExamples = () => {
    if (examples !== null || examplesLoading) return;
    setExamplesLoading(true);
    api
      .listExamples()
      .then((e) => setExamples(e))
      .catch(() => setExamples([]))
      .finally(() => setExamplesLoading(false));
  };

  // Document/file ops (New, Open, Save, Save As, Import, Download, Copy,
  // Validate, Add/Remove workflow). The hook owns the local UI state
  // they share — Save-As dialog draft + remove-workflow confirm flag —
  // and returns stable callbacks; the Toolbar stays a layout shell.
  const ops = useDocumentFileOps({ confirm });
  const {
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
  } = ops;

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
        fileInputRef.current?.click();
      } else if (e.key === "?" && !(e.target as HTMLElement).matches("input, textarea, select")) {
        e.preventDefault();
        setShowShortcuts(true);
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [undo, redo, handleSave]);

  const workflows = document?.workflows ?? [];

  // A team-authored cloud bot is a multi-file bundle: expose its files (skills,
  // manifest, …) via the Bundle-files drawer. Detected from the botsource://
  // virtual path the editor loads a tenant bot under.
  const bundleRef = api.parseBotSourceEditorPath(currentFilePath ?? "");
  const [bundleDrawerOpen, setBundleDrawerOpen] = useState(false);

  // "Duplicate & edit" for a read-only catalog bot open in the cloud editor:
  // fork it into the team's bot store and reopen the editable tenant copy.
  const { activeTeamID } = useAuth();
  const refetchBots = useBotsStore((s) => s.refetch);
  const { prompt: promptText, dialog: promptDialog } = usePromptText();
  const onDuplicateCatalog = async () => {
    const from = api.inferCatalogBotId(currentFilePath ?? "");
    if (!from) return;
    const slug = await promptText({
      title: `Duplicate ${from}`,
      message: "Copies this read-only catalog bot into an editable team bot.",
      label: "New bot id (lowercase, digits, - or _)",
      defaultValue: `${from}-copy`,
      confirmLabel: "Duplicate",
      validate: (v) =>
        /^[a-z0-9_-]+$/.test(v) ? null : "Use lowercase letters, digits, '-' or '_'",
    });
    if (!slug) return;
    try {
      const forked = await forkBotSource(activeTeamID, slug, from);
      await refetchBots();
      const file = botSourceEditorPath(activeTeamID, forked.slug);
      useTabsStore.getState().openTab("editor", { file });
      setLocation(`/editor?file=${encodeURIComponent(file)}`);
    } catch (err) {
      toastError(addToast, err, "Duplicate bot failed");
    }
  };

  return (
    <div className="flex items-center gap-1 px-4 h-10 text-sm bg-surface-1 border-b border-border-default">
      {/* File menu (VSCode-style unified dropdown) + inline primary Save */}
      <ToolbarGroup>
        <DropdownMenu
          side="bottom"
          align="start"
          onOpenChange={(open) => {
            if (open) loadExamples();
          }}
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
          <DropdownMenuItem icon={<FilePlusIcon />} shortcut="Ctrl+N" onSelect={handleNew}>
            New
          </DropdownMenuItem>
          <DropdownMenuItem icon={<UploadIcon />} shortcut="Ctrl+O" onSelect={openNative}>
            Open…
          </DropdownMenuItem>
          <DropdownMenuSub icon={<ClockIcon />} label="Open recent">
            {recents.length === 0 ? (
              <DropdownMenuItem disabled>No recent files</DropdownMenuItem>
            ) : (
              <>
                {recents.map((path) => (
                  <DropdownMenuItem
                    key={path}
                    onSelect={() => handlePickFile("file", path)}
                  >
                    {path}
                  </DropdownMenuItem>
                ))}
                <DropdownMenuSeparator />
                <DropdownMenuItem icon={<TrashIcon />} onSelect={clearRecents}>
                  Clear recents
                </DropdownMenuItem>
              </>
            )}
          </DropdownMenuSub>
          <DropdownMenuSub icon={<RocketIcon />} label="Examples">
            {examplesLoading && examples === null ? (
              <DropdownMenuItem disabled icon={<Spinner size="xs" />}>Loading…</DropdownMenuItem>
            ) : !examples || examples.length === 0 ? (
              <DropdownMenuItem disabled>No examples available</DropdownMenuItem>
            ) : (
              examples.map((name) => (
                <DropdownMenuItem
                  key={name}
                  onSelect={() => handlePickFile("example", name)}
                >
                  {name}
                </DropdownMenuItem>
              ))
            )}
          </DropdownMenuSub>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            icon={<DownloadIcon />}
            shortcut="Ctrl+S"
            disabled={!document}
            onSelect={handleSave}
          >
            Save
          </DropdownMenuItem>
          <DropdownMenuItem
            icon={<DownloadIcon />}
            disabled={!document}
            onSelect={handleSaveAsRequest}
          >
            Save As…
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            icon={<DownloadIcon />}
            disabled={!document}
            onSelect={handleDownload}
          >
            Download as .bot
          </DropdownMenuItem>
          <DropdownMenuItem
            icon={<CopyIcon />}
            disabled={!document}
            onSelect={handleCopySource}
          >
            Copy source to clipboard
          </DropdownMenuItem>
        </DropdownMenu>
        <Button
          variant="primary"
          size="sm"
          leadingIcon={<DownloadIcon />}
          onClick={handleSave}
          disabled={!document || readOnly}
          title={
            readOnly
              ? "Read-only catalog bot — use “Duplicate & edit” to make an editable copy"
              : currentFilePath
                ? `Save to ${currentFilePath} (Ctrl+S)`
                : "Save as… (Ctrl+S)"
          }
        >
          {readOnly ? "Read-only" : currentFilePath ? "Save" : "Save As"}
        </Button>
        {readOnly && (
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void onDuplicateCatalog()}
            title="Copy this catalog bot into an editable team bot"
          >
            Duplicate & edit
          </Button>
        )}
      </ToolbarGroup>

      {/* Bundle files (team-authored cloud bots only) */}
      {bundleRef && (
        <ToolbarGroup>
          <IconButton
            label="Bundle files"
            tooltip="Bundle files (skills, manifest…)"
            size="sm"
            onClick={() => setBundleDrawerOpen(true)}
          >
            <StackIcon />
          </IconButton>
        </ToolbarGroup>
      )}

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

      <input ref={fileInputRef} type="file" accept=".bot" className="hidden" onChange={handleImport} />

      {/* Right-aligned: file status → Launch run; then navigation icons */}
      <div className="ml-auto flex items-center gap-3">
        {loading && (
          <span className="text-xs text-fg-subtle inline-flex items-center gap-1">
            <Spinner size="xs" /> Loading…
          </span>
        )}
        <div className="flex items-center gap-2">
          <FileStatusBadge
            currentFilePath={currentFilePath}
            hasDocument={!!document}
            isDirty={isDirty()}
          />
          {backendProbed && !hasResolvedBackend && currentFilePath && (
            // Run is disabled for two reasons (no file / no credential) but
            // both share one tooltip. When the *credential* is the blocker,
            // surface a clickable nudge straight to Preferences → Backends —
            // otherwise the only signal is a silently greyed-out button.
            <IconButton
              variant="warning"
              size="sm"
              label="No LLM credential detected — open Preferences → Backends"
              tooltip="No LLM credential detected — click to open Preferences → Backends"
              onClick={() =>
                window.dispatchEvent(
                  new CustomEvent("iterion:open-settings", {
                    detail: { tab: "backends" },
                  }),
                )
              }
            >
              <ExclamationTriangleIcon />
            </IconButton>
          )}
          <Button
            variant="primary"
            size="sm"
            leadingIcon={<PlayIcon />}
            onClick={() =>
              setLocation(
                currentFilePath
                  ? `/runs/new?file=${encodeURIComponent(currentFilePath)}`
                  : // Unsaved buffer — launch off inline source (LaunchView
                    // reads the document store). The only launch path in cloud
                    // mode, where the pod rootfs is read-only and a workflow
                    // can never be saved to disk.
                    `/runs/new`,
              )
            }
            disabled={(!currentFilePath && !document) || !hasResolvedBackend}
            title={
              !currentFilePath && !document
                ? "Write or open a workflow first to launch a run"
                : !hasResolvedBackend
                ? "No LLM credentials detected — open Preferences → Backends to configure."
                : currentFilePath
                ? `Launch ${currentFilePath}`
                : "Launch the unsaved workflow"
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
      {bundleRef && (
        <BundleFilesDrawer
          teamID={bundleRef.teamID}
          slug={bundleRef.slug}
          open={bundleDrawerOpen}
          onOpenChange={setBundleDrawerOpen}
        />
      )}
      {promptDialog}
    </div>
  );
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
  // "bots/feature_dev › main.bot" with the parent slightly
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
        <span className="truncate text-caption text-fg-subtle font-mono shrink min-w-0">
          {displayParent}
        </span>
      )}
      {displayParent && (
        <span aria-hidden className="text-fg-subtle text-caption shrink-0">
          ›
        </span>
      )}
      <span className="truncate text-xs text-fg-default font-medium">
        {basename}
      </span>
      {subtitle && (
        <span className="text-caption text-fg-subtle shrink-0">{subtitle}</span>
      )}
    </div>
  );
}
