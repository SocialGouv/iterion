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
  // Opens the edit dialog for a still-editable (Backlog / ready-unlaunched)
  // ticket. Absent → no Edit affordance is shown.
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  // Opens the right-hand details sidebar for a card. Absent → cards are not
  // clickable and no Details affordance is shown.
  onOpenCard?: (card: PipelineBoardCardDTO) => void;
}

// isInteractiveClick reports whether a card click landed on (or inside) an
// interactive descendant — a link, button, or form control — rather than the
// card's inert chrome. Card-level clicks open the details sidebar; clicks on
// the title link or the footer/move buttons must keep doing their own thing,
// so those are ignored here. The walk stops at the card element itself (the
// handler's currentTarget).
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
    case "backlog":
      return "bg-fg-subtle";
    case "todo":
      return "bg-warning";
    case "in_progress":
      return "bg-info";
    case "done":
      return "bg-success";
    case "failed":
      return "bg-danger";
    default:
      return "bg-accent";
  }
}

// Ticket movement is BUTTON-driven — there is no drag & drop on this board.
// canMoveToTodo: a ticket-backed card that is not executing — a Backlog task
// being staged, or a Failed pipeline being retried. Running / paused /
// queued / finished runs are fixed by run state.
export function canMoveToTodo(card: PipelineBoardCardDTO): boolean {
  if (!card.issue_id) return false;
  if (card.column_id === "backlog") return card.kind === "task" || card.failed === true;
  if (card.column_id === "failed") return card.failed === true;
  return false;
}

// canMoveToBacklog: a ready-but-unlaunched Todo task goes back to Backlog. A
// queued RUN sitting in Todo is fixed by run state.
export function canMoveToBacklog(card: PipelineBoardCardDTO): boolean {
  return !!card.issue_id && card.column_id === "todo" && card.kind === "task";
}

// isTicketEditable reports whether a ticket can still be edited before it
// runs: it must be task-backed (issue_id) and not executing — a Backlog card,
// a Failed pipeline being fixed before retry, or a ready-but-unlaunched Todo
// task card. In-progress / done cards and queued runs are fixed.
export function isTicketEditable(card: PipelineBoardCardDTO): boolean {
  if (!card.issue_id) return false;
  return (
    card.column_id === "backlog" ||
    card.column_id === "failed" ||
    (card.column_id === "todo" && card.kind === "task")
  );
}

// moveTicket flips a ticket's ready state (true → Todo, staged for the launch
// loop; false → back to Backlog), then refetches the board.
export async function moveTicket(
  issueId: string,
  ready: boolean,
  onDone: () => void,
): Promise<void> {
  await markPipelineTaskReady(issueId, ready);
  onDone();
}

export function PipelineColumns({ board, onRefetch, onEditTask, onOpenCard }: Props) {
  const { columns, cards } = board;
  const [moveError, setMoveError] = useState<string | null>(null);

  const onMoveTicket = async (issueId: string, ready: boolean) => {
    setMoveError(null);
    try {
      await moveTicket(issueId, ready, onRefetch);
    } catch (e) {
      setMoveError(errorMessage(e));
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
      {moveError && (
        <div className="px-4 pt-2">
          <InlineBanner tone="danger" layout="inline">
            {moveError}
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
            onMoveTicket={onMoveTicket}
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
  onMoveTicket,
  onEditTask,
  onOpenCard,
}: {
  column: PipelineBoardColumn;
  cards: PipelineBoardCardDTO[];
  onMoveTicket: (issueId: string, ready: boolean) => void;
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  onOpenCard?: (card: PipelineBoardCardDTO) => void;
}) {
  return (
    <section
      className="flex max-h-full w-[21rem] shrink-0 flex-col overflow-hidden rounded-[var(--radius-lg)] border border-border-default bg-surface-2/70"
      aria-labelledby={`pipeline-column-${column.id}`}
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
              onMoveTicket={onMoveTicket}
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
  onMoveTicket: (issueId: string, ready: boolean) => void;
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  onOpenCard?: (card: PipelineBoardCardDTO) => void;
}

// PipelineCard is deliberately LEAN: title, status (progress / blocked / why),
// and the footer (run access, Details, Edit, move buttons). Everything else —
// inputs, produced elements, the review form, labels, description — lives in
// the details sidebar the card click opens.
export function PipelineCard({ card, onMoveTicket, onEditTask, onOpenCard }: CardProps) {
  const timestamp = card.updated_at || card.created_at;
  const editable = !!onEditTask && isTicketEditable(card);
  const openable = !!onOpenCard;

  return (
    <Card
      className={`space-y-2 p-3 ${openable ? "cursor-pointer" : ""}`}
      interactive={openable}
      data-card-id={card.id}
      role="article"
      aria-label={`${card.title}, ${humanizeToken(card.kind)}`}
      onClick={
        openable
          ? (e) => {
              // Clicks on the title link or footer buttons act on their own;
              // only inert-chrome clicks open the sidebar.
              if (isInteractiveClick(e)) return;
              onOpenCard?.(card);
            }
          : undefined
      }
    >
      <CardTitle card={card} />

      {card.column_id === "backlog" && <BacklogStatus card={card} />}
      {card.column_id === "todo" && <TodoStatus card={card} />}
      {card.column_id === "in_progress" && <InProgressStatus card={card} />}
      {card.column_id === "done" && <DoneStatus card={card} />}
      {card.column_id === "failed" && <FailedStatus card={card} />}

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
          {canMoveToTodo(card) && card.issue_id && (
            <button
              type="button"
              onClick={() => onMoveTicket(card.issue_id as string, true)}
              className="text-accent-text hover:underline"
              aria-label={`${card.failed ? "Retry" : "Move to Todo"}: ${card.title}`}
              title={
                card.failed
                  ? "Retry — stage this ticket back to Todo"
                  : "Stage this ticket: it starts when a slot frees"
              }
            >
              {card.failed ? "Retry" : "→ Todo"}
            </button>
          )}
          {canMoveToBacklog(card) && card.issue_id && (
            <button
              type="button"
              onClick={() => onMoveTicket(card.issue_id as string, false)}
              className="text-accent-text hover:underline"
              aria-label={`Back to Backlog: ${card.title}`}
              title="Unstage this ticket back to Backlog"
            >
              → Backlog
            </button>
          )}
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

// --- per-lane STATUS (the card's only body content) -------------------------

// PriorityBadge mirrors /board's P{n} chip. On this board the number is not
// just a sort key: the admission loop launches ready Todo tickets highest
// priority first (ties oldest-first), so P drives WHICH pipeline goes next.
function PriorityBadge({ priority }: { priority?: number }) {
  if (!priority || priority <= 0) return null;
  return (
    <Badge
      variant="warning"
      title={`Priority ${priority} — higher numbers launch first from Todo`}
    >
      P{priority}
    </Badge>
  );
}

// Backlog: why the ticket is here — failed (with the error) or being prepared.
function BacklogStatus({ card }: { card: PipelineBoardCardDTO }) {
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-1">
        <PriorityBadge priority={card.priority} />
        {card.failed && <Badge variant="danger">Failed</Badge>}
      </div>
      {card.failed && card.error && (
        <InlineBanner tone="danger" layout="inline">
          {card.error}
        </InlineBanner>
      )}
    </div>
  );
}

// Todo: queue position (waiting for a concurrency slot) or ready — plus the
// priority that decides its launch turn.
function TodoStatus({ card }: { card: PipelineBoardCardDTO }) {
  const queuePosition = card.queue_position ?? 0;
  return (
    <div className="flex flex-wrap items-center gap-1">
      <PriorityBadge priority={card.priority} />
      {queuePosition > 0 ? (
        <Badge variant="warning">Waiting · #{queuePosition}</Badge>
      ) : (
        <span className="text-micro text-fg-subtle">
          Ready — launches by priority when a slot frees
        </span>
      )}
    </div>
  );
}

// In progress: tree progress + a Blocked tag naming WHY (the pending human
// gate) when the pipeline waits on a review. The review form itself lives
// exclusively in the details sidebar. While blocked, the root's own status
// chip is NOISE — Blocked already says everything the operator can act on.
// A resumable process state (e.g. a restart orphaned the parked parent) is
// the SYSTEM's business, not the card's: the details sidebar explains it
// next to the review form, and #205 tracks making it self-heal entirely.
function InProgressStatus({ card }: { card: PipelineBoardCardDTO }) {
  const reviews = card.pending_reviews ?? [];
  const blockedLabel =
    reviews.length === 1
      ? `Blocked — human review${reviews[0]?.node_id ? ` · ${reviews[0].node_id}` : ""}`
      : `Blocked — ${reviews.length} human reviews`;
  return (
    <div className="space-y-2">
      <ProgressBar executed={card.tree_executed_nodes} total={card.tree_total_nodes} />
      <div className="flex flex-wrap items-center gap-1">
        {reviews.length > 0 ? (
          <Badge
            variant="warning"
            title="Waiting on a human review — open the card to answer it"
          >
            {blockedLabel}
          </Badge>
        ) : (
          <StatusChip status={card.status} />
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

// Done: just the terminal status — the output lives in the sidebar's Result.
function DoneStatus({ card }: { card: PipelineBoardCardDTO }) {
  return (
    <div className="flex flex-wrap items-center gap-1">
      <StatusChip status={card.status} />
    </div>
  );
}

// Failed: the terminal status + the failure REASON. Retry (→ Todo) and Edit
// live in the footer for ticket-backed cards; the priority shows because a
// retried ticket re-enters the Todo launch order with it.
function FailedStatus({ card }: { card: PipelineBoardCardDTO }) {
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-1">
        <PriorityBadge priority={card.priority} />
        <StatusChip status={card.status} />
      </div>
      {card.error && (
        <InlineBanner tone="danger" layout="inline">
          {card.error}
        </InlineBanner>
      )}
    </div>
  );
}
