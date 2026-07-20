// The scrollable column area of the board: flat per-state columns, or
// swimlanes when a grouping dimension is active. Owns the shared
// renderStateColumn/renderUnmapped builders so both layouts produce
// identical <Column>s; column-management affordances (header menu, reorder
// handle) are offered only in the flat view, so swimlane columns stay clean.

import type {
  DispatchSkipView,
  RetryView,
  RunningView,
} from "@/api/dispatcher";
import type { NativeBoard, NativeIssue, NativeState } from "@/api/native";

import { defaultStateColor } from "../boardShared";
import { Column } from "../Column";

import type { Lane } from "./useSwimlanes";
import type { UseColumnManagementResult } from "./useColumnManagement";

export function BoardColumnsArea({
  board,
  byState,
  swimlanes,
  selectedIds,
  runningByIssue,
  retryingByIssue,
  skipByIssue,
  dispatcherPaused,
  activeLabels,
  columns,
  onColumnDrop,
  onCardClick,
  onCardDragStart,
  onOpenCard,
  onSelectColumn,
  onLabelToggle,
  onCancelRun,
  onOpenRun,
  onClearSelection,
}: {
  board: NativeBoard;
  byState: Map<string, NativeIssue[]>;
  swimlanes: Lane[] | null;
  selectedIds: Set<string>;
  runningByIssue: Map<string, RunningView>;
  retryingByIssue: Map<string, RetryView>;
  skipByIssue: Map<string, DispatchSkipView>;
  dispatcherPaused: boolean;
  activeLabels: Set<string>;
  columns: UseColumnManagementResult;
  onColumnDrop: (ids: string[], toState: string) => void;
  onCardClick: (iss: NativeIssue, e: React.MouseEvent) => void;
  onCardDragStart: (iss: NativeIssue, e: React.DragEvent) => void;
  onOpenCard: (iss: NativeIssue) => void;
  onSelectColumn: (stateName: string) => void;
  onLabelToggle: (label: string) => void;
  onCancelRun: (issueID: string) => void;
  onOpenRun: (runId: string) => void;
  /** Fired when a click lands in empty board space while a selection exists. */
  onClearSelection: () => void;
}) {
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
      onOpenCard={onOpenCard}
      onSelectColumn={onSelectColumn}
      onLabelClick={onLabelToggle}
      activeLabels={activeLabels}
      onCancelRun={onCancelRun}
      onOpenRun={onOpenRun}
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
        onOpenCard={onOpenCard}
        onSelectColumn={onSelectColumn}
        onLabelClick={onLabelToggle}
        activeLabels={activeLabels}
        onCancelRun={onCancelRun}
        onOpenRun={onOpenRun}
        dimmed={dispatcherPaused}
      />
    ) : null;

  return (
    <div
      className="flex-1 overflow-auto p-3"
      // Click in the empty board area (column gaps, "drop here" space,
      // padding) clears the selection. Clicks landing on a card are
      // ignored here — the card carries data-issue-card and runs its
      // own selection handler.
      onClick={(e) => {
        if ((e.target as HTMLElement).closest("[data-issue-card]")) return;
        if (selectedIds.size > 0) onClearSelection();
      }}
    >
      {swimlanes ? (
        <div className="flex flex-col gap-4 min-w-fit">
          {/* Column mutations are ambiguous when the same state repeats
              across lanes, so the per-column menus are flat-view only —
              say so instead of looking broken. */}
          <div className="text-fg-subtle text-micro px-1">
            Column editing is available with Group: none.
          </div>
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
  );
}
