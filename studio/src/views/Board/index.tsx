import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import DispatcherControlBar from "@/components/shared/DispatcherControlBar";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Skeleton } from "@/components/ui/Skeleton";
import { cancelIssue } from "@/api/dispatcher";
import {
  createIssue,
  deleteIssue,
  patchIssue,
  type NativeIssue,
  type NativeState,
} from "@/api/native";
import IssueModal from "./IssueModal";
import { BoardFilters } from "./BoardFilters";
import { BoardKeyboardHelp } from "./BoardKeyboardHelp";
import { Column } from "./Column";
import {
  AddColumnDialog,
  DeleteColumnDialog,
  EditColumnDialog,
} from "./ColumnDialogs";
import { SelectionToolbar } from "./SelectionToolbar";
import SettingsDrawer from "@/components/Dispatcher/SettingsDrawer";
import TrackerErrorBanner from "@/components/shared/TrackerErrorBanner";
import { useBoardKeyboard } from "@/hooks/useBoardKeyboard";
import { useConfirm } from "@/hooks/useConfirm";
import { useToggleSet } from "@/hooks/useToggleSet";
import { useUIStore } from "@/store/ui";
import {
  defaultStateColor,
  type GroupMode,
  type SortMode,
} from "./boardShared";
import { EmptyBoard } from "./board/EmptyBoard";
import { EmptyBoardBanner } from "./board/EmptyBoardBanner";
import { isDispatchable } from "./board/boardSort";
import { useBoardData } from "./board/useBoardData";
import { useDispatcherPoll } from "./board/useDispatcherPoll";
import { useBoardColumns } from "./board/useBoardColumns";
import { useSwimlanes } from "./board/useSwimlanes";
import { useColumnManagement } from "./board/useColumnManagement";
import { useBoardSelection } from "./board/useBoardSelection";
import { useBoardDragDrop } from "./board/useBoardDragDrop";
import {
  useTransitionHistory,
  useUndoKeyboardShortcut,
} from "./board/useUndoTransitions";
import { useBoardBulkActions } from "./board/useBoardBulkActions";
import { useTabBadge } from "./board/useTabBadge";

export default function BoardView() {
  const [, setLocation] = useLocation();
  const search = useSearch();
  const focusFromUrl = useMemo(() => {
    return new URLSearchParams(search).get("focus");
  }, [search]);

  const { board, issues, setIssues, loading, error, setError, refresh } =
    useBoardData();

  const [editing, setEditing] = useState<NativeIssue | null>(null);
  const [creating, setCreating] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const {
    set: labelFilter,
    toggle: onLabelToggle,
    clear: clearLabelFilter,
  } = useToggleSet<string>();
  const [assigneeFilter, setAssigneeFilter] = useState("");
  const [sortMode, setSortMode] = useState<SortMode>("priority");
  const [groupMode, setGroupMode] = useState<GroupMode>("none");
  // `onLabelToggle` (from useToggleSet) is the single source of truth
  // for label filter toggling — used both by the top filter strip and
  // by clicking a chip on any card, so card-level chips toggle the
  // same Set the filter strip shows.

  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog: confirmDialog } = useConfirm();

  // Poll the dispatcher snapshot every 2s so each card can show a
  // running/retrying badge + cancel button. When the active (running +
  // retrying) set changes the hook re-fetches issues via setIssues so a
  // dispatched card actually moves columns instead of stranding.
  const {
    runningByIssue,
    retryingByIssue,
    skipByIssue,
    trackerError,
    dispatcherPaused,
  } = useDispatcherPoll(setIssues);

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
  const { filteredIssues, byState, flatIssueIds, allLabels, allAssignees } =
    useBoardColumns({
      board,
      issues,
      searchQuery,
      labelFilter,
      assigneeFilter,
      sortMode,
    });

  // Swimlanes: null when grouping is off (flat board), else the per-lane
  // grouping of the same filtered issues. Column management is offered only
  // in the flat view, so swimlane columns render without header menus.
  const swimlanes = useSwimlanes({ board, filteredIssues, groupMode, sortMode });

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

  const modalOpen = creating || editing !== null || helpOpen || settingsOpen;
  useUndoKeyboardShortcut({ historyRef, onDrop, modalOpen });

  const onCreate = useCallback(
    async (input: Partial<NativeIssue>) => {
      try {
        await createIssue({
          title: input.title ?? "",
          body: input.body,
          state: input.state,
          labels: input.labels,
          priority: input.priority,
          assignee: input.assignee,
          fields: input.fields,
          bot: input.bot,
          bot_args: input.bot_args,
        });
        setCreating(false);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [refresh, setError],
  );

  const onSave = useCallback(
    async (input: Partial<NativeIssue>) => {
      if (!editing) return;
      try {
        await patchIssue(editing.id, {
          title: input.title,
          body: input.body,
          labels: input.labels,
          priority: input.priority,
          // assignee/bot/bot_args all default to a cleared value ("" / "" /
          // {}) when the operator empties the field, so the corresponding
          // Patch pointer is SET and the server actually clears a
          // previously-stored value. The modal emits `undefined` for an
          // empty field; without the `?? ""` the key is JSON-dropped, the
          // server reads a nil pointer as "unchanged", and the stale value
          // silently persists. For `assignee` that also kept routing the
          // issue to the wrong per-assignee workflow (assignee selects the
          // bot), so clearing it has to reach the store.
          assignee: input.assignee ?? "",
          fields: input.fields,
          bot: input.bot ?? "",
          bot_args: input.bot_args ?? {},
        });
        setEditing(null);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [editing, refresh, setError],
  );

  const onDelete = useCallback(
    async (id: string) => {
      if (
        !(await confirm({
          title: "Delete this issue?",
          message: "This removes it from the board and cannot be undone.",
          confirmLabel: "Delete",
          confirmVariant: "danger",
        }))
      )
        return;
      try {
        await deleteIssue(id);
        setEditing(null);
        setSelectedIds((cur) => {
          if (!cur.has(id)) return cur;
          const next = new Set(cur);
          next.delete(id);
          return next;
        });
        setAnchorId((cur) => (cur === id ? null : cur));
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [confirm, refresh, setError, setSelectedIds, setAnchorId],
  );

  // The dispatch lane: the first eligible, non-terminal state (the
  // "Let's go"/ready column the dispatcher claims from). Falls back to
  // "ready" for boards that haven't flagged eligibility.
  const dispatchState = useMemo(
    () => board?.states.find((s) => s.eligible && !s.terminal)?.name ?? "ready",
    [board],
  );
  const selectedIssues = useMemo(
    () => issues.filter((i) => selectedIds.has(i.id)),
    [issues, selectedIds],
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
      <>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => setLocation("/board/labels")}
          title="Manage the board's label vocabulary"
        >
          Labels
        </Button>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => setLocation("/board/fields")}
          title="Manage the board's custom-field schema"
        >
          Fields
        </Button>
        <Button
          variant="secondary"
          size="sm"
          onClick={columns.openAddColumn}
          title="Add a new column (board state)"
        >
          + Add column
        </Button>
        <Button variant="secondary" size="sm" onClick={() => void refresh()}>
          Refresh
        </Button>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          + New issue
        </Button>
      </>
    ) : null,
  });

  if (loading) {
    return (
      <div
        className="h-full flex flex-col overflow-hidden"
        aria-label="Loading board"
      >
        <div className="flex-1 flex gap-3 overflow-hidden p-3">
          {Array.from({ length: 4 }).map((_, c) => (
            <div key={c} className="flex w-72 shrink-0 flex-col gap-2">
              <Skeleton className="h-6 w-32" />
              {Array.from({ length: 3 }).map((__, k) => (
                <Skeleton key={k} className="h-16 w-full" />
              ))}
            </div>
          ))}
        </div>
      </div>
    );
  }
  if (!board) {
    return <EmptyBoard kind="missing" />;
  }

  // Build the active column dialog at statement level so the discriminated
  // union narrows cleanly (it wouldn't inside JSX .map/.filter callbacks).
  const colDialog = columns.dialog;
  let columnDialogNode: React.ReactNode = null;
  if (colDialog.kind === "add") {
    columnDialogNode = (
      <AddColumnDialog
        existingNames={board.states.map((s) => s.name)}
        busy={columns.busy}
        error={columns.error}
        onCancel={columns.closeDialog}
        onSubmit={columns.submitAdd}
      />
    );
  } else if (colDialog.kind === "edit") {
    const st = colDialog.state;
    columnDialogNode = (
      <EditColumnDialog
        state={st}
        issueCount={columns.issueCount(st.name)}
        existingNames={board.states.map((s) => s.name).filter((n) => n !== st.name)}
        busy={columns.busy}
        error={columns.error}
        onCancel={columns.closeDialog}
        onSubmit={columns.submitEdit}
      />
    );
  } else if (colDialog.kind === "delete") {
    const st = colDialog.state;
    columnDialogNode = (
      <DeleteColumnDialog
        state={st}
        issueCount={columns.issueCount(st.name)}
        otherStates={board.states.filter((s) => s.name !== st.name)}
        isLast={board.states.length <= 1}
        busy={columns.busy}
        error={columns.error}
        onCancel={columns.closeDialog}
        onSubmit={columns.submitDelete}
      />
    );
  }

  // renderStateColumn builds a <Column> for a board state from a byState
  // map. Used by both the flat board and each swimlane; column-management
  // affordances (menu, reorder handle) are offered only in the flat view
  // (manage=true), so swimlane columns stay clean.
  const renderStateColumn = (
    s: NativeState,
    map: Map<string, NativeIssue[]>,
    manage: boolean,
    keyPrefix = "",
  ) => (
    <Column
      key={keyPrefix + s.name}
      name={s.name}
      display={s.display ?? s.name}
      terminal={!!s.terminal}
      eligible={!!s.eligible}
      color={s.color ?? defaultStateColor(s.name, !!s.eligible, !!s.terminal)}
      issues={map.get(s.name) ?? []}
      selectedIds={selectedIds}
      runningByIssue={runningByIssue}
      retryingByIssue={retryingByIssue}
      skipByIssue={skipByIssue}
      onDrop={onColumnDrop}
      onClickCard={onCardClick}
      onDragStartCard={onCardDragStart}
      onOpenCard={(iss) => setEditing(iss)}
      onSelectColumn={selectColumn}
      onLabelClick={onLabelToggle}
      activeLabels={labelFilter}
      onCancelRun={onCancelRun}
      onOpenRun={(runId) => setLocation(`/runs/${encodeURIComponent(runId)}`)}
      dimmed={dispatcherPaused}
      onEditColumn={manage ? columns.onEditColumn : undefined}
      onDeleteColumn={manage ? columns.onDeleteColumn : undefined}
      onMoveColumn={manage ? columns.onMoveColumn : undefined}
      onReorderColumn={manage ? columns.onReorderColumn : undefined}
    />
  );

  const renderUnmapped = (map: Map<string, NativeIssue[]>, keyPrefix = "") =>
    (map.get("__unmapped__")?.length ?? 0) > 0 ? (
      <Column
        key={keyPrefix + "__unmapped__"}
        name="__unmapped__"
        display="Unmapped"
        terminal={false}
        eligible={false}
        color="var(--color-board-backlog)"
        issues={map.get("__unmapped__") ?? []}
        selectedIds={selectedIds}
        runningByIssue={runningByIssue}
        retryingByIssue={retryingByIssue}
        skipByIssue={skipByIssue}
        onDrop={onColumnDrop}
        onClickCard={onCardClick}
        onDragStartCard={onCardDragStart}
        onOpenCard={(iss) => setEditing(iss)}
        onSelectColumn={selectColumn}
        onLabelClick={onLabelToggle}
        activeLabels={labelFilter}
        onCancelRun={onCancelRun}
        onOpenRun={(runId) => setLocation(`/runs/${encodeURIComponent(runId)}`)}
        dimmed={dispatcherPaused}
      />
    ) : null;

  return (
    <div className="h-full flex flex-col overflow-hidden">
      <DispatcherControlBar onOpenSettings={() => setSettingsOpen(true)} />
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
      {dispatcherPaused && (
        <InlineBanner tone="warning" title="Dispatcher paused">
          New issues won't be dispatched until you resume from the toolbar
          above. In-flight runs continue unaffected.
        </InlineBanner>
      )}

      <BoardFilters
        searchQuery={searchQuery}
        labelFilter={labelFilter}
        assigneeFilter={assigneeFilter}
        allLabels={allLabels}
        allAssignees={allAssignees}
        total={issues.length}
        filtered={filteredIssues.length}
        onSearchChange={setSearchQuery}
        onLabelToggle={onLabelToggle}
        onClearLabels={clearLabelFilter}
        onAssigneeChange={setAssigneeFilter}
        sortMode={sortMode}
        onSortChange={setSortMode}
        groupMode={groupMode}
        onGroupChange={setGroupMode}
        fieldNames={(board.fields ?? []).map((f) => f.name)}
        onReset={() => {
          setSearchQuery("");
          clearLabelFilter();
          setAssigneeFilter("");
          setGroupMode("none");
        }}
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
      <div
        className="flex-1 overflow-auto p-3"
        // Click in the empty board area (column gaps, "drop here" space,
        // padding) clears the selection. Clicks landing on a card are
        // ignored here — the card carries data-issue-card and runs its
        // own selection handler.
        onClick={(e) => {
          if ((e.target as HTMLElement).closest("[data-issue-card]")) return;
          if (selectedIds.size > 0) setSingleSelection(null);
        }}
      >
        {swimlanes ? (
          <div className="flex flex-col gap-4 min-w-fit">
            {swimlanes.length === 0 && (
              <div className="text-fg-muted text-xs p-4">
                No issues to group by this dimension.
              </div>
            )}
            {swimlanes.map((lane) => (
              <section key={lane.key} className="space-y-2">
                <h2 className="text-xs font-semibold text-fg-default flex items-center gap-2 sticky left-0">
                  <span className="uppercase tracking-wide">{lane.label}</span>
                  <span className="text-fg-muted font-normal">{lane.count}</span>
                </h2>
                <div className="flex gap-3 min-w-fit">
                  {board.states.map((s) =>
                    renderStateColumn(s, lane.byState, false, lane.key + "::"),
                  )}
                  {renderUnmapped(lane.byState, lane.key + "::")}
                </div>
              </section>
            ))}
          </div>
        ) : (
          <div className="flex gap-3 min-w-fit">
            {board.states.map((s) => renderStateColumn(s, byState, true))}
            {renderUnmapped(byState)}
          </div>
        )}
      </div>

      {creating && (
        <IssueModal
          board={board}
          initial={null}
          onSubmit={onCreate}
          onClose={() => setCreating(false)}
          allAssignees={allAssignees}
        />
      )}
      {editing && (
        <IssueModal
          board={board}
          initial={editing}
          allAssignees={allAssignees}
          onSubmit={onSave}
          onClose={() => setEditing(null)}
          onDelete={() => void onDelete(editing.id)}
          onDispatch={
            isDispatchable(editing.state)
              ? () => {
                  const id = editing.id;
                  setEditing(null);
                  void onDrop(id, dispatchState);
                  addToast("Dispatched 1 issue", "success");
                }
              : undefined
          }
        />
      )}
      {columnDialogNode}
      {confirmDialog}
      {helpOpen && <BoardKeyboardHelp onClose={() => setHelpOpen(false)} />}
    </div>
  );
}
