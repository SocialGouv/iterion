import { useState } from "react";
import { GripVertical, MoreVertical } from "lucide-react";

import { Checkbox } from "@/components/ui/Checkbox";
import { IconButton } from "@/components/ui/IconButton";
import { Popover, PopoverClose } from "@/components/ui/Popover";
import { formatRelative } from "@/lib/format";
import { clickableRowProps } from "@/lib/a11y";
import { softColor } from "@/lib/constants";
import type { DispatchSkipView, RetryView, RunningView } from "@/api/dispatcher";
import type { NativeIssue } from "@/api/native";

import { DRAG_MIME_ISSUE_IDS, DRAG_MIME_STATE } from "./boardShared";

// Max label chips shown on a card before collapsing the rest into "+N".
const MAX_CARD_LABELS = 3;

// TERMINAL_BOARD_STATES lists the native-tracker state names treated as
// "no more work" for UI purposes. The runtime contract is that any
// state with `terminal: true` in the board config qualifies — but the
// card doesn't carry the board's flag here, so we hard-code the
// canonical names. Keep in sync with the defaults in
// pkg/dispatcher/native/board.go's NewStore (done + blocked + cancelled).
const TERMINAL_BOARD_STATES = new Set(["done", "blocked", "cancelled"]);

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
                  aria-label={`Manage ${display} column`}
                  title="Column options"
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
        {issues.length === 0 && (
          <div className="text-xs text-fg-muted text-center py-4">drop here</div>
        )}
      </div>
    </div>
  );
}

interface IssueCardProps {
  iss: NativeIssue;
  selected: boolean;
  running?: RunningView;
  retrying?: RetryView;
  // skip: present when the dispatcher refused to claim this eligible
  // issue because its explicit `bot` is unresolvable / unrouteable.
  // Rendered as a warning badge so the stall is visible + actionable.
  skip?: DispatchSkipView;
  // activeLabels: the set of labels currently in the board-level
  // filter, so each card's label chip can show its active state and
  // operators can see which chips already filter the view.
  activeLabels: Set<string>;
  // onClick receives the mouse event so the parent can update the
  // selection (plain click = select; Shift / Ctrl / Meta = multi-select).
  onClick: (e: React.MouseEvent) => void;
  // onOpen opens the issue modal — triggered by a double-click on the
  // card or a plain click on the title text (GitHub-style).
  onOpen: () => void;
  // onDragStart receives the drag event so the parent can decide
  // whether to drag this card alone or the whole multi-selection
  // and write the right payload into dataTransfer.
  onDragStart: (e: React.DragEvent) => void;
  onLabelClick: (label: string) => void;
  onCancelRun: () => void;
  onOpenRun: (runId: string) => void;
  onShowRetryDetails: () => void;
}

function IssueCard({
  iss,
  selected,
  running,
  retrying,
  skip,
  activeLabels,
  onClick,
  onOpen,
  onDragStart,
  onLabelClick,
  onCancelRun,
  onOpenRun,
  onShowRetryDetails,
}: IssueCardProps) {
  // Hover preview: synthesise a multi-line title combining body
  // (truncated) + key fields + blocker count so the OS-native tooltip
  // provides a quick peek without forcing a modal open. Title strings
  // render with newlines on all major browsers.
  const previewLines: string[] = [];
  if (iss.body) {
    const trimmed = iss.body.trim();
    previewLines.push(trimmed.length > 240 ? trimmed.slice(0, 237) + "…" : trimmed);
  }
  if (iss.fields && Object.keys(iss.fields).length > 0) {
    previewLines.push(
      Object.entries(iss.fields)
        .map(([k, v]) => `${k}: ${String(v)}`)
        .join("\n"),
    );
  }
  if (iss.blockers && iss.blockers.length > 0) {
    previewLines.push(`Blocked by: ${iss.blockers.join(", ")}`);
  }
  const hoverTitle = previewLines.length > 0 ? previewLines.join("\n\n") : undefined;
  const [dragging, setDragging] = useState(false);
  const pinnedFields = iss.fields ? pickPinnedFields(iss.fields) : [];
  return (
    <div
      {...clickableRowProps(onOpen, iss.title)}
      draggable
      data-issue-card
      title={hoverTitle}
      onDragStart={(e) => {
        onDragStart(e);
        setDragging(true);
      }}
      onDragEnd={() => setDragging(false)}
      onClick={onClick}
      onDoubleClick={onOpen}
      className={`bg-surface-0 border rounded p-2 text-sm cursor-grab active:cursor-grabbing transition-transform ${
        dragging ? "scale-[1.02] shadow-[var(--shadow-lg)]" : ""
      } ${
        selected
          ? "border-accent ring-1 ring-accent/40"
          : "border-border-default hover:border-accent/40"
      }`}
    >
      <div className="flex items-start gap-2">
        <span
          // GitHub-style: the title text is the affordance that opens
          // the modal. A plain click here opens; a modified click falls
          // through to the card's selection handler for multi-select.
          className="text-fg-default flex-1 cursor-pointer hover:underline"
          onClick={(e) => {
            if (e.ctrlKey || e.metaKey || e.shiftKey) return;
            e.stopPropagation();
            onOpen();
          }}
        >
          {iss.title}
        </span>
        {iss.priority && iss.priority > 0 ? (
          <span className="text-caption px-1.5 py-0.5 rounded bg-warning-soft text-warning-fg">
            P{iss.priority}
          </span>
        ) : null}
      </div>
      {pinnedFields.length > 0 && (
        <div className="mt-0.5 flex items-center gap-2 text-caption text-fg-subtle flex-wrap">
          {pinnedFields.map(([k, v]) => (
            <span key={k} className="flex items-center gap-1">
              <span className="font-mono opacity-70">{k}:</span>
              <span className="text-fg-default">{String(v)}</span>
            </span>
          ))}
        </div>
      )}
      {iss.labels && iss.labels.length > 0 && (
        <div className="mt-1 flex flex-wrap gap-1">
          {iss.labels.slice(0, MAX_CARD_LABELS).map((l) => {
            const palette = labelPalette(l);
            const active = activeLabels.has(l);
            return (
              <button
                key={l}
                type="button"
                // Stop propagation so a chip click only toggles the
                // board's label filter — without this the card's
                // onClick would also open the issue modal, which is
                // not what the operator asked for.
                onClick={(e) => {
                  e.stopPropagation();
                  onLabelClick(l);
                }}
                className={`text-caption px-1.5 py-0.5 rounded hover:ring-1 hover:ring-accent transition ${
                  active ? "ring-1 ring-accent" : ""
                }`}
                style={palette}
                title={
                  active
                    ? `Click to remove ${l} from the board filter`
                    : `Click to filter board by ${l}`
                }
              >
                {l}
              </button>
            );
          })}
          {iss.labels.length > MAX_CARD_LABELS && (
            <span
              className="text-caption px-1.5 py-0.5 rounded bg-surface-2 text-fg-subtle"
              title={iss.labels.slice(MAX_CARD_LABELS).join(", ")}
            >
              +{iss.labels.length - MAX_CARD_LABELS}
            </span>
          )}
        </div>
      )}
      <div className="mt-1 flex items-center gap-2 text-caption text-fg-muted flex-wrap">
        <code className="opacity-70">{shortID(iss.id)}</code>
        {iss.bot && (
          <span
            className="font-mono bg-accent/15 text-accent-text rounded px-1 py-0.5"
            title={`Will dispatch via ${iss.bot} (overrides dispatcher config)`}
          >
            🤖 {iss.bot}
          </span>
        )}
        {iss.assignee && <span>@{iss.assignee}</span>}
        {iss.claim && (
          <span
            className="text-warning-fg"
            title={`Locked by ${iss.claim} — the dispatcher holds the claim until the run finishes.`}
          >
            claimed by {iss.claim}
          </span>
        )}
        {!running && iss.last_run_id && (() => {
          const lastRunId = iss.last_run_id;
          return (
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onOpenRun(lastRunId);
              }}
              className="font-mono text-info hover:underline opacity-80"
              title={`Open the last run on this issue (run ${lastRunId})`}
            >
              ↪ last run
            </button>
          );
        })()}
        {iss.updated_at && (
          <span className="text-fg-subtle" title={iss.updated_at}>
            · updated {formatRelative(iss.updated_at)}
          </span>
        )}
      </div>
      {running && (
        <div className="mt-1 flex items-center justify-between gap-2 rounded bg-success-soft px-1.5 py-1 text-caption text-success-fg">
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onOpenRun(running.run_id);
            }}
            className="text-left flex-1 hover:underline cursor-pointer"
            title={
              running.attempt && running.attempt > 0
                ? `Open run ${running.run_id} (resume of a prior failed_resumable run — attempt ${running.attempt + 1})`
                : `Open run ${running.run_id}`
            }
          >
            ● {running.attempt && running.attempt > 0 ? "resuming" : "running"}
            {running.attempt && running.attempt > 0 ? (
              <span className="ml-1 text-warning-fg/90">#{running.attempt + 1}</span>
            ) : null}
            {running.last_event_name && (
              <span className="ml-1 text-success-fg/70">— {running.last_event_name}</span>
            )}
          </button>
          <button
            className="rounded border border-success/40 px-1.5 py-0.5 text-caption hover:bg-success-soft"
            onClick={(e) => {
              e.stopPropagation();
              onCancelRun();
            }}
            title="Cancel this in-flight run"
          >
            cancel
          </button>
        </div>
      )}
      {!running && retrying && !TERMINAL_BOARD_STATES.has(iss.state) && (
        <button
          type="button"
          className="mt-1 w-full text-left rounded bg-warning-soft px-1.5 py-1 text-caption text-warning-fg cursor-pointer hover:bg-warning-soft"
          onClick={(e) => {
            e.stopPropagation();
            onShowRetryDetails();
          }}
          title={retrying.error ? `Last error: ${retrying.error}` : undefined}
        >
          ⏳ retrying (attempt {retrying.attempt})
          {retrying.error && (
            <span className="ml-1 text-warning-fg/80 truncate">— {retrying.error}</span>
          )}
        </button>
      )}
      {!running && retrying && TERMINAL_BOARD_STATES.has(iss.state) && (
        <div
          className="mt-1 rounded bg-fg-muted/10 px-1.5 py-1 text-caption text-fg-subtle"
          title={`The dispatcher still has a retry entry for this issue, but it's in a terminal state (${iss.state}) — the retry will be skipped on the next tick.`}
        >
          stale retry queued — will be skipped (issue in {iss.state})
        </div>
      )}
      {!running && skip && (
        <button
          type="button"
          className="mt-1 w-full text-left rounded bg-danger-soft px-1.5 py-1 text-caption text-danger-fg cursor-pointer hover:bg-danger-soft"
          onClick={(e) => {
            e.stopPropagation();
            onOpen();
          }}
          title={`The dispatcher refuses to run this issue: ${skip.reason}. Fix the bot in the issue editor or add it to assignee_workflows.`}
        >
          ⚠ won&apos;t dispatch
          <span className="ml-1 text-danger-fg/80 truncate">— {skip.reason}</span>
        </button>
      )}
    </div>
  );
}

// labelPalette derives a stable pastel background + foreground colour
// from a label name. Two cards with the label "urgent" always render
// the same colour, but "infra" and "urgent" land on visibly distinct
// palettes. Hashing avoids the need for a label-colour schema in the
// backend — operators get colour scanning today without configuration.
// A small alias table covers common semantic labels with sensible
// presets (red for "urgent" / "bug", green for "ready", etc.).
// Token-driven alias table at module scope: built once, not per-label
// per-card per-render. Severity labels reuse the prebuilt design-system
// *-soft pairs (single source of truth for the 18% tint); docs borrows the
// iteration-1 (purple) hue, which has no -soft token, via softColor. The
// chips invert correctly in light mode because the values are CSS vars.
const DANGER_CHIP = { backgroundColor: "var(--color-danger-soft)", color: "var(--color-danger-fg)" };
const SUCCESS_CHIP = { backgroundColor: "var(--color-success-soft)", color: "var(--color-success-fg)" };
const LABEL_ALIASES: Record<string, { backgroundColor: string; color: string }> = {
  urgent: DANGER_CHIP,
  blocker: DANGER_CHIP,
  bug: DANGER_CHIP,
  infra: { backgroundColor: "var(--color-info-soft)", color: "var(--color-info-fg)" },
  docs: { backgroundColor: softColor("var(--color-iteration-1)", 18), color: "var(--color-fg-default)" },
  feature: SUCCESS_CHIP,
  ready: SUCCESS_CHIP,
};

function labelPalette(label: string): { backgroundColor: string; color: string } {
  const hit = LABEL_ALIASES[label.toLowerCase()];
  if (hit) return hit;
  // Stable 32-bit FNV-1a hash → hue. Fixed S/L keeps the palette readable
  // against both light and dark surfaces.
  let h = 2166136261 >>> 0;
  for (let i = 0; i < label.length; i++) {
    h ^= label.charCodeAt(i);
    h = Math.imul(h, 16777619) >>> 0;
  }
  const hue = h % 360;
  return {
    backgroundColor: `hsl(${hue}, 60%, 28%)`,
    color: `hsl(${hue}, 80%, 88%)`,
  };
}

// pickPinnedFields returns up to two scalar field entries from a card's
// `fields` map so the card body can surface high-signal data (enum
// statuses, customer IDs) inline without expanding the modal. Skips
// fields whose value is too long for a card row — those belong in the
// hover preview / modal view.
function pickPinnedFields(fields: Record<string, unknown>): Array<[string, unknown]> {
  const picked: Array<[string, unknown]> = [];
  for (const [k, v] of Object.entries(fields)) {
    if (picked.length >= 2) break;
    if (v === null || v === undefined) continue;
    if (typeof v === "object") continue;
    const str = String(v);
    if (str.length === 0 || str.length > 32) continue;
    picked.push([k, v]);
  }
  return picked;
}

function shortID(id: string) {
  const bare = id.replace(/^native:/, "").replace(/^github:[^#]+#/, "#");
  return bare.length > 10 ? bare.slice(0, 10) : bare;
}
