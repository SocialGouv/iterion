import { Link } from "wouter";

import type {
  PipelineBoard,
  PipelineBoardCard as PipelineBoardCardDTO,
  PipelineBoardColumn,
} from "@/api/pipelineBoards";
import { resumeRun } from "@/api/runs";
import type { UnifiedStatus } from "@/components/Runs/runStatusClasses";
import { Badge, Button, Card, InlineBanner, StatusBadge } from "@/components/ui";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { formatRelative } from "@/lib/format";

import { SequentialReviews } from "./SequentialReviews";

interface Props {
  board: PipelineBoard;
  onRefetch: () => void;
}

// Statuses that resume from a preserved checkpoint. Reuses the run console's
// resume path (resumeRun) — the same call OperatorPauseBanner / the run
// console use.
const RESUMABLE_STATUSES = new Set(["failed_resumable", "cancelled", "paused_operator"]);

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
    case "todo":
      return "bg-fg-subtle";
    case "in_progress":
      return "bg-info";
    case "done":
      return "bg-success";
    case "attention":
      return "bg-danger";
    default:
      return "bg-accent";
  }
}

export function PipelineColumns({ board, onRefetch }: Props) {
  const { columns, cards } = board;
  return (
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
          onRefetch={onRefetch}
        />
      ))}
    </div>
  );
}

function PipelineColumn({
  column,
  cards,
  onRefetch,
}: {
  column: PipelineBoardColumn;
  cards: PipelineBoardCardDTO[];
  onRefetch: () => void;
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
            <PipelineCard key={card.id} card={card} onRefetch={onRefetch} />
          ))
        )}
      </div>
    </section>
  );
}

interface CardProps {
  card: PipelineBoardCardDTO;
  onRefetch: () => void;
}

export function PipelineCard({ card, onRefetch }: CardProps) {
  const timestamp = card.updated_at || card.created_at;
  return (
    <Card
      className="space-y-2 p-3"
      data-card-id={card.id}
      role="article"
      aria-label={`${card.title}, ${humanizeToken(card.kind)}`}
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

      {card.column_id === "todo" && <TodoBody card={card} />}
      {card.column_id === "in_progress" && (
        <InProgressBody card={card} onRefetch={onRefetch} />
      )}
      {card.column_id === "done" && <DoneBody card={card} />}
      {card.column_id === "attention" && (
        <AttentionBody card={card} onRefetch={onRefetch} />
      )}

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
        {timestamp && (
          <span className="ml-auto text-fg-subtle" title={timestamp}>
            {formatRelative(timestamp)}
          </span>
        )}
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

// --- TODO lane ------------------------------------------------------------

function TodoBody({ card }: { card: PipelineBoardCardDTO }) {
  const queuePosition = card.queue_position ?? 0;
  return (
    <div className="space-y-2">
      <EntryInput input={card.entry_input} />
      <div className="flex flex-wrap items-center gap-1">
        {queuePosition > 0 ? (
          <Badge variant="warning">Waiting · #{queuePosition}</Badge>
        ) : card.kind === "task" ? (
          <span className="text-micro text-fg-subtle">Not launched</span>
        ) : null}
        {card.priority !== undefined && card.priority !== 0 && (
          <Badge variant="accent">P{card.priority}</Badge>
        )}
        {card.issue_state && (
          <Badge variant="neutral">{humanizeToken(card.issue_state)}</Badge>
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

function InProgressBody({
  card,
  onRefetch,
}: {
  card: PipelineBoardCardDTO;
  onRefetch: () => void;
}) {
  if (card.pending_reviews && card.pending_reviews.length > 0) {
    return <SequentialReviews card={card} onResolved={onRefetch} />;
  }
  const descendants = card.descendant_count ?? 0;
  return (
    <div className="space-y-2">
      <ProgressBar executed={card.tree_executed_nodes} total={card.tree_total_nodes} />
      <div className="flex flex-wrap items-center gap-1">
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

// --- ATTENTION lane -------------------------------------------------------

function AttentionBody({
  card,
  onRefetch,
}: {
  card: PipelineBoardCardDTO;
  onRefetch: () => void;
}) {
  const resumeAction = useAsyncAction();
  const canResume = !!card.run_id && RESUMABLE_STATUSES.has(card.status ?? "");

  const resume = async () => {
    const runID = card.run_id;
    if (!runID) return;
    const result = await resumeAction.run(() => resumeRun(runID));
    if (result !== undefined) onRefetch();
  };

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-1">
        <StatusChip status={card.status} />
      </div>
      {card.error && (
        <InlineBanner tone="danger" layout="inline">
          {card.error}
        </InlineBanner>
      )}
      {canResume && (
        <div className="space-y-2 rounded-md border border-info/40 bg-info-soft p-2">
          <p className="text-micro text-info-fg">
            Resume this pipeline from its persisted checkpoint.
          </p>
          {resumeAction.error && (
            <InlineBanner tone="danger" layout="inline">
              {resumeAction.error}
            </InlineBanner>
          )}
          <Button
            variant="secondary"
            size="sm"
            loading={resumeAction.busy}
            onClick={() => void resume()}
          >
            Resume run
          </Button>
        </div>
      )}
    </div>
  );
}
