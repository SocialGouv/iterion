// BoardView — the kanban orchestrator. Wires the board/ concern modules
// together: data (useBoardData), filters + saved views (useBoardFilters →
// BoardFilterBar), dispatcher overlay (useDispatcherPoll), per-column
// derivations (useBoardColumns/useSwimlanes), selection + drag-drop + undo,
// column management (useColumnManagement → ColumnDialogHost), issue CRUD
// (useIssueActions → IssueModalHost), bulk actions and keyboard shortcuts.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import { cancelIssue } from "@/api/dispatcher";
import DispatcherControlBar from "@/components/shared/DispatcherControlBar";
import TrackerErrorBanner from "@/components/shared/TrackerErrorBanner";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import SettingsDrawer from "@/components/Dispatcher/SettingsDrawer";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useBoardKeyboard } from "@/hooks/useBoardKeyboard";
import { useConfirm } from "@/hooks/useConfirm";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import { BoardKeyboardHelp } from "./BoardKeyboardHelp";
import { SelectionToolbar } from "./SelectionToolbar";
import { BoardColumnsArea } from "./board/BoardColumnsArea";
import { BoardFilterBar } from "./board/BoardFilterBar";
import { BoardHeaderActions } from "./board/BoardHeaderActions";
import { BoardSkeleton } from "./board/BoardSkeleton";
import { ColumnDialogHost } from "./board/ColumnDialogHost";
import { EmptyBoard } from "./board/EmptyBoard";
import { EmptyBoardBanner } from "./board/EmptyBoardBanner";
import { IssueModalHost } from "./board/IssueModalHost";
import { isDispatchable } from "./board/boardSort";
import { useBoardBulkActions } from "./board/useBoardBulkActions";
import { useBoardColumns } from "./board/useBoardColumns";
import { useBoardData } from "./board/useBoardData";
import { useBoardDragDrop } from "./board/useBoardDragDrop";
import { useBoardFilters } from "./board/useBoardFilters";
import { useBoardSelection } from "./board/useBoardSelection";
import { useColumnManagement } from "./board/useColumnManagement";
import { useDispatcherPoll } from "./board/useDispatcherPoll";
import { useIssueActions } from "./board/useIssueActions";
import { useSwimlanes } from "./board/useSwimlanes";
import { useTabBadge } from "./board/useTabBadge";
import {
  useTransitionHistory,
  useUndoKeyboardShortcut,
} from "./board/useUndoTransitions";

export default function BoardView() {
  const [, setLocation] = useLocation();
  const search = useSearch();
  const focusFromUrl = useMemo(() => {
    return new URLSearchParams(search).get("focus");
  }, [search]);

  const { board, issues, setIssues, loading, error, setError, refresh } =
    useBoardData();

  const [settingsOpen, setSettingsOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);

  // Filter/sort/group + saved-view ("views bar") state. filters.onLabelToggle
  // is the single toggle used by both the filter strip and card-level chips.
  const filters = useBoardFilters({ refresh });

  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog: confirmDialog } = useConfirm();

  // Poll the dispatcher snapshot every 2s so each card can show a
  // running/retrying badge + cancel button. When the active (running +
  // retrying) set changes the hook re-fetches issues via setIssues so a
  // dispatched card actually moves columns instead of stranding.
  // The dispatcher (`iterion dispatch`) is a self-hosted feature; in cloud
  // mode it's disabled and its /api/v1/dispatcher/* endpoints aren't wired.
  // Gate every dispatcher surface on this so the board doesn't surface a
  // scary "Can't reach the dispatcher API" for a feature that's simply off.
  const dispatcherEnabled = useServerInfoStore((s) => s.info?.dispatcher_enabled ?? false);
  const {
    runningByIssue,
    retryingByIssue,
    skipByIssue,
    trackerError,
    dispatcherPaused,
  } = useDispatcherPoll(setIssues, dispatcherEnabled);

  const onCancelRun = useCallback(
    async (issueID: string) => {
      try {
        await cancelIssue(issueID);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [setError],
  );

  // Derived per-column data (filter → group-by-state → sort + the
  // flat issue-id sequence used for shift-click range selection).
  const { filteredIssues, byState, flatIssueIds, allLabels, allAssignees, allBots } =
    useBoardColumns({
      board,
      issues,
      searchQuery: filters.searchQuery,
      labelFilter: filters.labelFilter,
      assigneeFilter: filters.assigneeFilter,
      botFilter: filters.botFilter,
      sortMode: filters.sortMode,
      repoScope: filters.repoScope,
      includeUnlinked: filters.includeUnlinked,
    });

  // Swimlanes: null when grouping is off (flat board), else the per-lane
  // grouping of the same filtered issues.
  const swimlanes = useSwimlanes({
    board,
    filteredIssues,
    groupMode: filters.groupMode,
    sortMode: filters.sortMode,
  });

  // Column (state) management: header menu + reorder drag + add/edit/
  // delete dialogs. Mutations refresh the board+issues afterward.
  const columns = useColumnManagement({ board, issues, refresh });

  // Multi-selection state + click/drag-start selection logic.
  const {
    selectedIds,
    setSelectedIds,
    setAnchorId,
    anchorId,
    setSingleSelection,
    toggleSelection,
    selectAllVisible,
    selectColumn,
    onCardClick,
    onCardDragStart,
  } = useBoardSelection({ filteredIssues, flatIssueIds, byState });

  // Apply the ?focus=<issueID> deep-link from the Dispatcher view's
  // retry-queue rows. Runs once after issues load so the auto-selected
  // card is actually present in state. Self-clears the param so a hard
  // reload doesn't re-focus on an issue the user has since moved on
  // from.
  useEffect(() => {
    if (!focusFromUrl) return;
    if (issues.length === 0) return;
    const match = issues.find((i) => i.id === focusFromUrl);
    if (!match) return;
    setSingleSelection(match.id);
    setLocation("/board", { replace: true });
  }, [focusFromUrl, issues, setLocation, setSingleSelection]);

  // Mirror eligible-state counts into the browser tab title so a pinned
  // board surfaces new ready/in-progress work without focusing the tab.
  useTabBadge({ board, byState });

  // Bounded transition history (Ctrl+Z target). The keyboard shortcut
  // itself is wired below, AFTER onDrop exists — splitting the history
  // ref from the undo handler is what breaks the dragDrop↔undo cycle.
  const { recordTransition, historyRef } = useTransitionHistory();

  const { onDrop, onColumnDrop } = useBoardDragDrop({
    setIssues,
    setError,
    recordTransition,
  });

  // Issue-modal glue: create/edit state + the CRUD handlers.
  const { creating, setCreating, editing, setEditing, onCreate, onSave, onDelete } =
    useIssueActions({ refresh, setError, confirm, setSelectedIds, setAnchorId });

  const modalOpen = creating || editing !== null || helpOpen || settingsOpen;
  useUndoKeyboardShortcut({ historyRef, onDrop, modalOpen });

  // The staging/dispatch lane: the first eligible, non-terminal state
  // (the "Let's go"/ready column). The dispatcher claims from it when
  // enabled; otherwise cards staged here launch their assigned bot.
  // Falls back to "ready" for boards that haven't flagged eligibility.
  const dispatchState = useMemo(
    () => board?.states.find((s) => s.eligible && !s.terminal)?.name ?? "ready",
    [board],
  );
  const selectedIssues = useMemo(
    () => issues.filter((i) => selectedIds.has(i.id)),
    [issues, selectedIds],
  );
  // Offer the "Repository" swimlane grouping only when some card is
  // forge-linked (carries external.repo).
  const hasRepoLinks = useMemo(
    () => issues.some((i) => !!i.external?.repo),
    [issues],
  );
  const allSelectedDispatchable =
    selectedIssues.length > 0 && selectedIssues.every((i) => isDispatchable(i.state));

  const {
    onBulkDispatch,
    onBulkMove,
    onBulkPriority,
    onBulkAssignee,
    onBulkToggleLabel,
    onBulkDelete,
  } = useBoardBulkActions({
    board,
    selectedIssues,
    dispatchState,
    dispatcherEnabled,
    onDrop,
    refresh,
    setError,
    setSingleSelection,
    addToast,
    confirm,
    setLocation,
  });

  useBoardKeyboard({
    board,
    byState,
    selectedId: anchorId,
    modalOpen,
    onSelect: setSingleSelection,
    onToggleSelect: toggleSelection,
    onSelectAllVisible: selectAllVisible,
    onCreate: () => setCreating(true),
    onEdit: (id) => {
      const iss = issues.find((i) => i.id === id);
      if (iss) setEditing(iss);
    },
    onDelete: (id) => void onDelete(id),
    onTransition: (id, toState) => void onDrop(id, toState),
    onShowHelp: () => setHelpOpen((v) => !v),
  });

  useHeaderSlot({
    left: <span className="text-xs font-medium text-fg-default">Board</span>,
    right: board ? (
      <BoardHeaderActions
        onManageLabels={() => setLocation("/board/labels")}
        onManageFields={() => setLocation("/board/fields")}
        onAddColumn={columns.openAddColumn}
        onRefresh={() => void refresh()}
        onNewIssue={() => setCreating(true)}
      />
    ) : null,
  });

  if (loading) {
    return <BoardSkeleton />;
  }
  if (!board) {
    return <EmptyBoard kind="missing" error={error} onRetry={() => void refresh()} />;
  }

  return (
    <div className="h-full flex flex-col overflow-hidden">
      {dispatcherEnabled && (
        <DispatcherControlBar onOpenSettings={() => setSettingsOpen(true)} />
      )}
      <SettingsDrawer
        open={settingsOpen}
        onClose={() => setSettingsOpen(false)}
        onSaved={() => void refresh()}
      />

      {error && <InlineBanner tone="danger">{error}</InlineBanner>}
      {trackerError && (
        <TrackerErrorBanner
          tracker={trackerError.tracker}
          message={trackerError.message}
        />
      )}
      {dispatcherEnabled && dispatcherPaused && (
        <InlineBanner tone="warning" title="Dispatcher paused">
          New issues won't be dispatched until you resume from the toolbar
          above. In-flight runs continue unaffected.
        </InlineBanner>
      )}

      <BoardFilterBar
        filters={filters}
        board={board}
        allLabels={allLabels}
        allAssignees={allAssignees}
        allBots={allBots}
        total={issues.length}
        filtered={filteredIssues.length}
        hasRepoLinks={hasRepoLinks}
      />

      {issues.length === 0 && (
        <EmptyBoardBanner onCreate={() => setCreating(true)} />
      )}
      {selectedIds.size > 0 && (
        <SelectionToolbar
          count={selectedIds.size}
          board={board}
          allLabels={allLabels}
          allAssignees={allAssignees}
          selectedIssues={selectedIssues}
          allSelectedDispatchable={allSelectedDispatchable}
          onDispatch={() => void onBulkDispatch()}
          onMove={(s) => void onBulkMove(s)}
          onPriority={onBulkPriority}
          onAssignee={onBulkAssignee}
          onToggleLabel={onBulkToggleLabel}
          onDelete={() => void onBulkDelete()}
          onClear={() => setSingleSelection(null)}
        />
      )}

      <BoardColumnsArea
        board={board}
        byState={byState}
        swimlanes={swimlanes}
        selectedIds={selectedIds}
        runningByIssue={runningByIssue}
        retryingByIssue={retryingByIssue}
        skipByIssue={skipByIssue}
        dispatcherPaused={dispatcherPaused}
        activeLabels={filters.labelFilter}
        columns={columns}
        onColumnDrop={onColumnDrop}
        onCardClick={onCardClick}
        onCardDragStart={onCardDragStart}
        onOpenCard={setEditing}
        onSelectColumn={selectColumn}
        onLabelToggle={filters.onLabelToggle}
        onCancelRun={onCancelRun}
        onOpenRun={(runId) => setLocation(`/runs/${encodeURIComponent(runId)}`)}
        onClearSelection={() => setSingleSelection(null)}
      />

      <IssueModalHost
        board={board}
        creating={creating}
        editing={editing}
        allAssignees={allAssignees}
        onCreate={onCreate}
        onSave={onSave}
        onCloseCreate={() => setCreating(false)}
        onCloseEdit={() => setEditing(null)}
        onDelete={(id) => void onDelete(id)}
        onDispatch={(id) => {
          void onDrop(id, dispatchState);
          addToast(
            dispatcherEnabled ? "Dispatched 1 issue" : "Staged 1 issue",
            "success",
          );
        }}
      />
      <ColumnDialogHost board={board} columns={columns} />
      {confirmDialog}
      {helpOpen && <BoardKeyboardHelp onClose={() => setHelpOpen(false)} />}
    </div>
  );
}
