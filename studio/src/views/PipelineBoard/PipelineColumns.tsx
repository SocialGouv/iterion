import { useState } from "react";
import { Link } from "wouter";

import type {
  PipelineBoard,
  PipelineBoardCard as PipelineBoardCardDTO,
  PipelineBoardColumn,
} from "@/api/pipelineBoards";
import { markPipelineTaskReady } from "@/api/pipelineBoards";
import type { UnifiedStatus } from "@/components/Runs/runStatusClasses";
import { Badge, Card, InlineBanner, StatusBadge } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";

interface Props {
  board: PipelineBoard;
  onRefetch: () => void;
  // Opens the edit dialog for a still-editable (Draft / ready-unlaunched)
  // ticket. Absent → no Edit affordance is shown.
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  // Opens the right-hand details sidebar for a card. Absent → cards are not
  // clickable and no Details affordance is shown.
  onOpenCard?: (card: PipelineBoardCardDTO) => void;
}

// isInteractiveClick reports whether a card click landed on (or inside) an
// interactive descendant — a link, button, or form control — rather than the
// card's inert chrome. Card-level clicks open the details sidebar; clicks on
// the title link, the Edit button, or an inline review form must keep doing
// their own thing, so those are ignored here. The walk stops at the card
// element itself (the handler's currentTarget).
function isInteractiveClick(e: React.MouseEvent): boolean {
  const boundary = e.currentTarget;
  let node = e.target as HTMLElement | null;
  while (node && node !== boundary) {
    const tag = node.tagName;
    if (
      tag === "A" ||
      tag === "BUTTON" ||
      tag === "INPUT" ||
      tag === "TEXTAREA" ||
      tag === "SELECT" ||
      tag === "LABEL" ||
      node.getAttribute("role") === "button" ||
      node.hasAttribute("data-no-card-open")
    ) {
      return true;
    }
    node = node.parentElement;
  }
  return false;
}

// The MIME the Draft ↔ Todo drag carries — the ticket's issue_id.
const DRAG_MIME = "text/plain";

const KNOWN_STATUSES = new Set<UnifiedStatus>([
  "running",
  "paused_waiting_human",
  "paused_operator",
  "finished",
  "failed",
  "failed_resumable",
  "cancelled",
  "queued",
  "skipped",
  "none",
]);

function isKnownStatus(status: string): status is UnifiedStatus {
  return KNOWN_STATUSES.has(status as UnifiedStatus);
}

function humanizeToken(value: string): string {
  const label = value.replace(/[_-]+/g, " ").trim();
  return label ? label.charAt(0).toUpperCase() + label.slice(1) : value;
}

function columnAccent(id: string): string {
  switch (id) {
    case "draft":
      return "bg-fg-subtle";
    case "todo":
      return "bg-warning";
    case "in_progress":
      return "bg-info";
    case "done":
      return "bg-success";
    default:
      return "bg-accent";
  }
}

// isTicketDraggable reports whether a card can be dragged between Draft and
// Todo. Only task-backed tickets that are NOT currently executing move: a
// not-yet-launched task (kind "task") or a failed ticket (retry by dragging
// to Todo). Running / paused / queued / finished runs are fixed by run state.
export function isTicketDraggable(card: PipelineBoardCardDTO): boolean {
  if (!card.issue_id) return false;
  return card.kind === "task" || card.failed === true;
}

// readyStateForDropColumn maps a drop-target column to the ready flag the
// write should set — true for Todo, false for Draft, null for a column that
// is not a drop target (in_progress / done).
export function readyStateForDropColumn(columnId: string): boolean | null {
  if (columnId === "todo") return true;
  if (columnId === "draft") return false;
  return null;
}

// isTicketEditable reports whether a ticket can still be edited before it
// runs: it must be task-backed (issue_id) and not executing — a Draft card
// (a task, or a failed run being retried) or a ready-but-unlaunched Todo task
// card. In-progress / done cards and queued runs are fixed.
export function isTicketEditable(card: PipelineBoardCardDTO): boolean {
  if (!card.issue_id) return false;
  return (
    card.column_id === "draft" ||
    (card.column_id === "todo" && card.kind === "task")
  );
}

// dropTicketToColumn performs the ready-state write for a Draft ↔ Todo drop,
// then refetches the board. A no-op for a non-drop-target column.
export async function dropTicketToColumn(
  issueId: string,
  columnId: string,
  onDone: () => void,
): Promise<void> {
  const ready = readyStateForDropColumn(columnId);
  if (ready === null) return;
  await markPipelineTaskReady(issueId, ready);
  onDone();
}

export function PipelineColumns({ board, onRefetch, onEditTask, onOpenCard }: Props) {
  const { columns, cards } = board;
  const [dropError, setDropError] = useState<string | null>(null);

  const onDropTicket = async (issueId: string, columnId: string) => {
    setDropError(null);
    try {
      await dropTicketToColumn(issueId, columnId, onRefetch);
    } catch (e) {
      setDropError(errorMessage(e));
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
      {dropError && (
        <div className="px-4 pt-2">
          <InlineBanner tone="danger" layout="inline">
            {dropError}
          </InlineBanner>
        </div>
      )}
      <div
        className="flex min-h-0 flex-1 items-start gap-3 overflow-x-auto px-4 pb-4"
        role="region"
        aria-label="Pipeline board columns"
      >
        {columns.map((column) => (
          <PipelineColumn
            key={column.id}
            column={column}
            cards={cards.filter((card) => card.column_id === column.id)}
            onDropTicket={onDropTicket}
            onEditTask={onEditTask}
            onOpenCard={onOpenCard}
          />
        ))}
      </div>
    </div>
  );
}

function PipelineColumn({
  column,
  cards,
  onDropTicket,
  onEditTask,
  onOpenCard,
}: {
  column: PipelineBoardColumn;
  cards: PipelineBoardCardDTO[];
  onDropTicket: (issueId: string, columnId: string) => void;
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  onOpenCard?: (card: PipelineBoardCardDTO) => void;
}) {
  const [dragOver, setDragOver] = useState(false);
  const droppable = readyStateForDropColumn(column.id) !== null;

  return (
    <section
      className={`flex max-h-full w-[21rem] shrink-0 flex-col overflow-hidden rounded-[var(--radius-lg)] border bg-surface-2/70 ${
        dragOver ? "border-accent ring-1 ring-accent/40" : "border-border-default"
      }`}
      aria-labelledby={`pipeline-column-${column.id}`}
      onDragOver={
        droppable
          ? (e) => {
              e.preventDefault();
              setDragOver(true);
            }
          : undefined
      }
      onDragLeave={droppable ? () => setDragOver(false) : undefined}
      onDrop={
        droppable
          ? (e) => {
              e.preventDefault();
              setDragOver(false);
              const id = e.dataTransfer.getData(DRAG_MIME);
              if (id) onDropTicket(id, column.id);
            }
          : undefined
      }
    >
      <div className="relative shrink-0 border-b border-border-default bg-surface-1 px-3 py-2.5">
        <span
          className={`absolute inset-x-0 top-0 h-0.5 ${columnAccent(column.id)}`}
          aria-hidden
        />
        <div className="flex items-center gap-2">
          <h2
            id={`pipeline-column-${column.id}`}
            className="min-w-0 flex-1 truncate text-xs font-semibold text-fg-default"
            title={column.title}
          >
            {column.title}
          </h2>
          <Badge variant="neutral">{cards.length}</Badge>
        </div>
      </div>

      <div className="min-h-24 flex-1 space-y-2 overflow-y-auto p-2">
        {cards.length === 0 ? (
          <div className="flex min-h-20 items-center justify-center rounded-md border border-dashed border-border-default px-3 text-center text-micro text-fg-subtle">
            No cards here
          </div>
        ) : (
          cards.map((card) => (
            <PipelineCard
              key={card.id}
              card={card}
              onEditTask={onEditTask}
              onOpenCard={onOpenCard}
            />
          ))
        )}
      </div>
    </section>
  );
}

interface CardProps {
  card: PipelineBoardCardDTO;
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  onOpenCard?: (card: PipelineBoardCardDTO) => void;
}

export function PipelineCard({ card, onEditTask, onOpenCard }: CardProps) {
  const timestamp = card.updated_at || card.created_at;
  const draggable = isTicketDraggable(card);
  const editable = !!onEditTask && isTicketEditable(card);
  const openable = !!onOpenCard;

  return (
    <Card
      className={`space-y-2 p-3 ${draggable ? "cursor-grab active:cursor-grabbing" : ""} ${
        openable && !draggable ? "cursor-pointer" : ""
      }`}
      interactive={openable}
      data-card-id={card.id}
      role="article"
      aria-label={`${card.title}, ${humanizeToken(card.kind)}`}
      onClick={
        openable
          ? (e) => {
              // Clicks on the title link, Edit / Details buttons, or an inline
              // review form act on their own; only inert-chrome clicks open
              // the sidebar.
              if (isInteractiveClick(e)) return;
              onOpenCard?.(card);
            }
          : undefined
      }
      draggable={draggable || undefined}
      onDragStart={
        draggable && card.issue_id
          ? (e) => {
              e.dataTransfer.setData(DRAG_MIME, card.issue_id as string);
              e.dataTransfer.effectAllowed = "move";
            }
          : undefined
      }
    >
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <CardTitle card={card} />
          <div className="mt-0.5 flex flex-wrap items-center gap-1 text-caption text-fg-subtle">
            {card.bot_id && <code title={`Bot ${card.bot_id}`}>{card.bot_id}</code>}
            {card.issue_id && <code title={`Issue ${card.issue_id}`}>#{card.issue_id}</code>}
            {card.workflow_name && <span className="truncate">{card.workflow_name}</span>}
          </div>
        </div>
        <Badge variant={card.kind === "run" ? "info" : "neutral"}>
          {humanizeToken(card.kind)}
        </Badge>
      </div>

      {card.body && (
        <p className="line-clamp-3 whitespace-pre-wrap text-xs text-fg-muted">{card.body}</p>
      )}

      {card.column_id === "draft" && <DraftBody card={card} />}
      {card.column_id === "todo" && <TodoBody card={card} />}
      {card.column_id === "in_progress" && <InProgressBody card={card} />}
      {card.column_id === "done" && <DoneBody card={card} />}

      {card.labels && card.labels.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {card.labels.map((label) => (
            <Badge key={label} variant="neutral">
              {label}
            </Badge>
          ))}
        </div>
      )}

      <div className="flex items-center gap-2 border-t border-border-default pt-2 text-micro">
        {card.run_id ? (
          <Link
            href={`/runs/${encodeURIComponent(card.run_id)}`}
            className="font-mono text-accent-text hover:underline"
            title={`Open run ${card.run_id}`}
            aria-label={`Open run ${card.run_id} in the run console`}
          >
            {card.run_id.slice(0, 12)}
          </Link>
        ) : (
          <span className="text-fg-subtle">Not started</span>
        )}
        <span className="ml-auto flex items-center gap-2">
          {openable && (
            <button
              type="button"
              onClick={() => onOpenCard?.(card)}
              className="text-accent-text hover:underline"
              aria-label={`Details for ${card.title}`}
            >
              Details
            </button>
          )}
          {editable && (
            <button
              type="button"
              onClick={() => onEditTask?.(card)}
              className="text-accent-text hover:underline"
              aria-label={`Edit ticket ${card.title}`}
            >
              Edit
            </button>
          )}
          {timestamp && (
            <span className="text-fg-subtle" title={timestamp}>
              {formatRelative(timestamp)}
            </span>
          )}
        </span>
      </div>
    </Card>
  );
}

// CardTitle links to the run console when the card is backed by a run;
// otherwise it is plain text (a not-yet-launched native task).
function CardTitle({ card }: { card: PipelineBoardCardDTO }) {
  if (card.run_id) {
    return (
      <Link
        href={`/runs/${encodeURIComponent(card.run_id)}`}
        className="text-sm font-medium leading-snug text-fg-default hover:underline"
      >
        {card.title}
      </Link>
    );
  }
  return (
    <div className="text-sm font-medium leading-snug text-fg-default">{card.title}</div>
  );
}

function StatusChip({ status }: { status?: string }) {
  if (!status) return null;
  return isKnownStatus(status) ? (
    <StatusBadge status={status} />
  ) : (
    <Badge>{status}</Badge>
  );
}

// --- DRAFT lane -----------------------------------------------------------

function DraftBody({ card }: { card: PipelineBoardCardDTO }) {
  return (
    <div className="space-y-2">
      <EntryInput input={card.entry_input} />
      <div className="flex flex-wrap items-center gap-1">
        {card.failed && <Badge variant="danger">Failed</Badge>}
        {card.priority !== undefined && card.priority !== 0 && (
          <Badge variant="accent">P{card.priority}</Badge>
        )}
        {card.issue_state && !card.failed && (
          <Badge variant="neutral">{humanizeToken(card.issue_state)}</Badge>
        )}
      </div>
      {card.failed && card.error && (
        <InlineBanner tone="danger" layout="inline">
          {card.error}
        </InlineBanner>
      )}
      <p className="text-micro text-fg-subtle">
        {card.failed
          ? "Drag to Todo to retry."
          : "Drag to Todo when this ticket is ready to run."}
      </p>
    </div>
  );
}

// --- TODO lane ------------------------------------------------------------

function TodoBody({ card }: { card: PipelineBoardCardDTO }) {
  const queuePosition = card.queue_position ?? 0;
  return (
    <div className="space-y-2">
      <EntryInput input={card.entry_input} />
      <div className="flex flex-wrap items-center gap-1">
        {queuePosition > 0 ? (
          <Badge variant="warning">Waiting · #{queuePosition}</Badge>
        ) : (
          <span className="text-micro text-fg-subtle">
            Ready — starts when a slot frees
          </span>
        )}
        {card.priority !== undefined && card.priority !== 0 && (
          <Badge variant="accent">P{card.priority}</Badge>
        )}
      </div>
    </div>
  );
}

// EntryInput renders the pipeline's launch vars / task bot-args as a compact
// key: value list.
function EntryInput({ input }: { input?: Record<string, unknown> }) {
  const entries = input ? Object.entries(input) : [];
  if (entries.length === 0) return null;
  return (
    <dl className="space-y-0.5 text-micro">
      {entries.map(([key, value]) => (
        <div key={key} className="flex min-w-0 gap-1">
          <dt className="shrink-0 font-medium text-fg-muted">{key}:</dt>
          <dd className="min-w-0 truncate text-fg-subtle" title={stringifyValue(value)}>
            {stringifyValue(value)}
          </dd>
        </div>
      ))}
    </dl>
  );
}

function stringifyValue(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

// --- IN_PROGRESS lane -----------------------------------------------------

// The card stays a STATUS surface: progress + badges only. The human-review
// form lives exclusively in the details sidebar (open the card) — a pending
// gate shows here as a "Blocked" tag, never as an inline form.
function InProgressBody({ card }: { card: PipelineBoardCardDTO }) {
  const descendants = card.descendant_count ?? 0;
  const reviews = card.pending_reviews?.length ?? 0;
  return (
    <div className="space-y-2">
      <ProgressBar executed={card.tree_executed_nodes} total={card.tree_total_nodes} />
      <div className="flex flex-wrap items-center gap-1">
        {reviews > 0 && (
          <Badge
            variant="warning"
            title="Waiting on a human review — open the card to answer it"
          >
            Blocked — {reviews > 1 ? `${reviews} human reviews` : "human review"}
          </Badge>
        )}
        <StatusChip status={card.status} />
        {descendants > 0 && (
          <Badge variant="neutral">
            +{descendants} child{descendants === 1 ? "" : "ren"}
          </Badge>
        )}
      </div>
    </div>
  );
}

function ProgressBar({ executed, total }: { executed: number; total: number }) {
  const pct = total > 0 ? Math.min(100, Math.round((executed / total) * 100)) : 0;
  return (
    <div className="flex flex-col gap-1">
      <div
        className="h-1.5 w-full overflow-hidden rounded-full bg-surface-3"
        role="progressbar"
        aria-valuenow={executed}
        aria-valuemin={0}
        aria-valuemax={total}
      >
        <div
          className="h-full rounded-full bg-info transition-all"
          style={{ width: `${pct}%` }}
        />
      </div>
      <div className="text-micro tabular-nums text-fg-subtle">
        {executed} / {total} nodes
      </div>
    </div>
  );
}

// --- DONE lane ------------------------------------------------------------

function DoneBody({ card }: { card: PipelineBoardCardDTO }) {
  if (!card.output) {
    return <p className="text-micro italic text-fg-subtle">No output.</p>;
  }
  return (
    <pre className="max-h-40 overflow-auto whitespace-pre rounded-md border border-border-default bg-surface-1 p-2 font-mono text-micro text-fg-muted">
      {card.output}
    </pre>
  );
}
