import { useState } from "react";
import { GripVertical, MoreVertical } from "lucide-react";

import { Checkbox } from "@/components/ui/Checkbox";
import { IconButton } from "@/components/ui/IconButton";
import { Popover, PopoverClose } from "@/components/ui/Popover";
import type { DispatchSkipView, RetryView, RunningView } from "@/api/dispatcher";
import type { NativeIssue } from "@/api/native";

import { DRAG_MIME_ISSUE_IDS, DRAG_MIME_STATE } from "./boardShared";
import { IssueCard } from "./IssueCard";

interface ColumnProps {
  name: string;
  display: string;
  terminal: boolean;
  eligible: boolean;
  // Hex or CSS color string used to tint the column header strip and the
  // count chip. Always provided by the parent — either from State.Color
  // (board config) or from `defaultStateColor()` (semantic fallback).
  color: string;
  issues: NativeIssue[];
  selectedIds: Set<string>;
  runningByIssue: Map<string, RunningView>;
  retryingByIssue: Map<string, RetryView>;
  skipByIssue: Map<string, DispatchSkipView>;
  // onDrop receives the dropped issue ids (one or more, parsed
  // from the dataTransfer payload) and the destination state name.
  onDrop: (ids: string[], toState: string) => void;
  // onClickCard receives the mouse event so the parent can inspect
  // Shift / Ctrl / Meta modifiers to drive multi-select.
  onClickCard: (iss: NativeIssue, e: React.MouseEvent) => void;
  // onDragStartCard lets the parent decide whether to drag just this
  // card or the full multi-selection, and write the appropriate
  // payload into dataTransfer.
  onDragStartCard: (iss: NativeIssue, e: React.DragEvent) => void;
  // onOpenCard opens the modal directly (used by in-card buttons
  // like "retry details" that should always open regardless of any
  // active selection modifier).
  onOpenCard: (iss: NativeIssue) => void;
  // onSelectColumn toggles the whole column in/out of the selection
  // (the header select-all checkbox).
  onSelectColumn: (stateName: string) => void;
  onLabelClick: (label: string) => void;
  activeLabels: Set<string>;
  onCancelRun: (issueID: string) => void;
  onOpenRun: (runId: string) => void;
  // dimmed: tells the column to render at reduced opacity. Used when the
  // dispatcher is paused so eligible columns visually fade — the cards
  // are still draggable, but the user gets a clear "nothing will pick
  // these up" signal.
  dimmed?: boolean;
  // Column-management callbacks (operator-only). Absent for the synthetic
  // "__unmapped__" column, which renders no header menu / drag handle.
  onEditColumn?: (name: string) => void;
  onDeleteColumn?: (name: string) => void;
  // onMoveColumn nudges a column one slot left/right (keyboard-free reorder).
  onMoveColumn?: (name: string, dir: "left" | "right") => void;
  // onReorderColumn fires when another column header is dropped onto this
  // one: move `dragged` to this column's position.
  onReorderColumn?: (dragged: string, target: string) => void;
}

export function Column({
  name,
  display,
  terminal,
  eligible,
  color,
  issues,
  selectedIds,
  runningByIssue,
  retryingByIssue,
  skipByIssue,
  onDrop,
  onClickCard,
  onDragStartCard,
  onOpenCard,
  onSelectColumn,
  onLabelClick,
  activeLabels,
  onCancelRun,
  onOpenRun,
  dimmed,
  onEditColumn,
  onDeleteColumn,
  onMoveColumn,
  onReorderColumn,
}: ColumnProps) {
  const [dragOver, setDragOver] = useState(false);
  const manageable = name !== "__unmapped__";
  const selCount = issues.reduce((n, i) => n + (selectedIds.has(i.id) ? 1 : 0), 0);
  const allSelected = issues.length > 0 && selCount === issues.length;
  // Dim only the eligible columns when the dispatcher is paused — the
  // terminal / backlog columns aren't being actively dispatched even
  // when the dispatcher runs, so muting them carries no extra signal.
  const fadeForPause = dimmed && eligible;
  return (
    <div
      className={`w-72 shrink-0 rounded border-2 transition-colors ${
        dragOver
          ? "border-accent bg-accent-soft/30 ring-2 ring-accent/40"
          : "border-border-default bg-surface-1"
      } flex flex-col ${fadeForPause ? "opacity-60" : ""}`}
      style={{ borderTopColor: color, borderTopWidth: 3 }}
      onDragOver={(e) => {
        e.preventDefault();
        setDragOver(true);
      }}
      onDragLeave={() => setDragOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDragOver(false);
        if (name === "__unmapped__") return;
        // Column reorder takes precedence: a header drag carries the
        // dragged state name, which must not be mistaken for a card drop.
        const draggedState = e.dataTransfer.getData(DRAG_MIME_STATE);
        if (draggedState) {
          if (draggedState !== name) onReorderColumn?.(draggedState, name);
          return;
        }
        const json = e.dataTransfer.getData(DRAG_MIME_ISSUE_IDS);
        if (json) {
          try {
            const ids = JSON.parse(json) as unknown;
            if (Array.isArray(ids) && ids.every((x) => typeof x === "string") && ids.length > 0) {
              onDrop(ids as string[], name);
              return;
            }
          } catch {
            // malformed payload — fall through to text/plain
          }
        }
        const single = e.dataTransfer.getData("text/plain");
        if (single) onDrop([single], name);
      }}
    >
      <div
        className="px-3 py-2 border-b border-border-default flex items-center justify-between text-xs"
        // The header is the drag handle for column reorder. Made
        // draggable only for manageable columns; the grip icon signals it.
        draggable={manageable && !!onReorderColumn}
        onDragStart={(e) => {
          if (!manageable || !onReorderColumn) return;
          e.dataTransfer.setData(DRAG_MIME_STATE, name);
          e.dataTransfer.effectAllowed = "move";
        }}
      >
        <span className="flex items-center gap-2 min-w-0">
          {manageable && onReorderColumn && (
            <GripVertical
              className="h-3.5 w-3.5 shrink-0 text-fg-subtle cursor-grab"
              aria-hidden="true"
            />
          )}
          {manageable && issues.length > 0 && (
            <Checkbox
              checked={allSelected}
              ref={(el) => {
                if (el) el.indeterminate = selCount > 0 && !allSelected;
              }}
              onChange={() => onSelectColumn(name)}
              title={allSelected ? "Deselect all in column" : "Select all in column"}
              aria-label={allSelected ? `Deselect all in ${display}` : `Select all in ${display}`}
              className="shrink-0 cursor-pointer"
            />
          )}
          <span
            className="inline-block h-2 w-2 rounded-full shrink-0"
            style={{ backgroundColor: color }}
            aria-hidden="true"
          />
          <span className="font-semibold uppercase tracking-wide text-fg-default truncate">
            {display}
          </span>
        </span>
        <span className="text-fg-muted flex items-center gap-1">
          {selCount > 0 && (
            <span className="text-accent-text font-medium">{selCount} sel ·</span>
          )}
          {issues.length}
          {eligible && <span className="ml-1 text-success">●</span>}
          {terminal && <span className="ml-1 text-fg-muted">✓</span>}
          {manageable && (onEditColumn || onDeleteColumn || onMoveColumn) && (
            <Popover
              align="end"
              trigger={
                <IconButton
                  size="sm"
                  variant="ghost"
                  label={`Manage ${display} column`}
                  tooltip="Column options"
                  className="ml-1"
                  // Don't let the header's dragstart hijack a menu click.
                  draggable={false}
                  onDragStart={(e) => e.preventDefault()}
                >
                  <MoreVertical className="h-4 w-4" />
                </IconButton>
              }
              contentClassName="p-1 min-w-40 text-xs"
            >
              <div className="flex flex-col">
                {onEditColumn && (
                  <PopoverClose asChild>
                    <button
                      type="button"
                      className="text-left px-2 py-1.5 rounded hover:bg-surface-2 text-fg-default"
                      onClick={() => onEditColumn(name)}
                    >
                      Edit column…
                    </button>
                  </PopoverClose>
                )}
                {onMoveColumn && (
                  <>
                    <PopoverClose asChild>
                      <button
                        type="button"
                        className="text-left px-2 py-1.5 rounded hover:bg-surface-2 text-fg-default"
                        onClick={() => onMoveColumn(name, "left")}
                      >
                        Move left
                      </button>
                    </PopoverClose>
                    <PopoverClose asChild>
                      <button
                        type="button"
                        className="text-left px-2 py-1.5 rounded hover:bg-surface-2 text-fg-default"
                        onClick={() => onMoveColumn(name, "right")}
                      >
                        Move right
                      </button>
                    </PopoverClose>
                  </>
                )}
                {onDeleteColumn && (
                  <PopoverClose asChild>
                    <button
                      type="button"
                      className="text-left px-2 py-1.5 rounded hover:bg-surface-2 text-danger-fg"
                      onClick={() => onDeleteColumn(name)}
                    >
                      Delete column…
                    </button>
                  </PopoverClose>
                )}
              </div>
            </Popover>
          )}
        </span>
      </div>
      <div className="p-2 flex-1 flex flex-col gap-2 overflow-auto">
        {issues.map((iss) => (
          <IssueCard
            key={iss.id}
            iss={iss}
            selected={selectedIds.has(iss.id)}
            running={runningByIssue.get(iss.id)}
            retrying={retryingByIssue.get(iss.id)}
            skip={skipByIssue.get(iss.id)}
            activeLabels={activeLabels}
            onClick={(e) => onClickCard(iss, e)}
            onOpen={() => onOpenCard(iss)}
            onDragStart={(e) => onDragStartCard(iss, e)}
            onLabelClick={onLabelClick}
            onCancelRun={() => onCancelRun(iss.id)}
            onOpenRun={onOpenRun}
            onShowRetryDetails={() => onOpenCard(iss)}
          />
        ))}
        {issues.length === 0 &&
          // The "drop here" affordance only makes sense mid-drag; an idle
          // empty column shows a muted placeholder instead.
          (dragOver ? (
            <div className="text-xs text-accent-text text-center py-4">Drop here</div>
          ) : (
            <div className="text-xs text-fg-subtle text-center py-4">No issues</div>
          ))}
      </div>
    </div>
  );
}
