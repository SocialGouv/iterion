import { Link } from "wouter";

import type {
  PipelineBoardCard as PipelineBoardCardDTO,
  PipelineBoardColumn,
} from "@/api/pipelineBoards";
import { resumeRun } from "@/api/runs";
import HumanPromptForm from "@/components/Runs/conversation/HumanPromptForm";
import type { UnifiedStatus } from "@/components/Runs/runStatusClasses";
import {
  Badge,
  Button,
  Card,
  InlineBanner,
  StatusBadge,
} from "@/components/ui";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { formatRelative } from "@/lib/format";

interface Props {
  columns: PipelineBoardColumn[];
  cards: PipelineBoardCardDTO[];
  onChanged: () => void;
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

function columnAccent(kind: string): string {
  switch (kind) {
    case "todo":
      return "bg-fg-subtle";
    case "running":
      return "bg-info";
    case "interaction":
    case "human":
      return "bg-warning";
    case "done":
    case "finished":
      return "bg-success";
    case "failed":
    case "attention":
      return "bg-danger";
    default:
      return "bg-accent";
  }
}

export function PipelineColumns({ columns, cards, onChanged }: Props) {
  const knownColumnIDs = new Set(columns.map((column) => column.id));
  const unknownColumnIDs = Array.from(
    new Set(cards.map((card) => card.column_id).filter((id) => !knownColumnIDs.has(id))),
  );
  // Never hide a card because a server topology projection is stale. Unknown
  // buckets are appended, visibly labelled, until the next poll repairs it.
  const visibleColumns: PipelineBoardColumn[] = [
    ...columns,
    ...unknownColumnIDs.map((id) => ({ id, title: id, kind: "unmapped" })),
  ];
  const cardsByRunID = new Map(
    cards.flatMap((card) => (card.run_id ? [[card.run_id, card] as const] : [])),
  );

  return (
    <div
      className="flex min-h-0 flex-1 items-start gap-3 overflow-x-auto px-4 pb-4"
      role="region"
      aria-label="Pipeline board columns"
    >
      {visibleColumns.map((column) => {
        const columnCards = cards.filter((card) => card.column_id === column.id);
        return (
          <section
            key={column.id}
            className="flex max-h-full w-[21rem] shrink-0 flex-col overflow-hidden rounded-[var(--radius-lg)] border border-border-default bg-surface-2/70"
            aria-labelledby={`pipeline-column-${column.id}`}
          >
            <div className="relative shrink-0 border-b border-border-default bg-surface-1 px-3 py-2.5">
              <span
                className={`absolute inset-x-0 top-0 h-0.5 ${columnAccent(column.kind)}`}
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
                <Badge variant="neutral">{columnCards.length}</Badge>
              </div>
              {(column.node_id || column.workflow_name) && (
                <div className="mt-1 flex min-w-0 items-center gap-1 text-caption text-fg-subtle">
                  {column.node_id && (
                    <code className="truncate" title={column.node_id}>
                      {column.node_id}
                    </code>
                  )}
                  {column.node_id && column.workflow_name && <span>·</span>}
                  {column.workflow_name && (
                    <span className="truncate" title={column.workflow_name}>
                      {column.workflow_name}
                    </span>
                  )}
                </div>
              )}
            </div>

            <div className="min-h-24 flex-1 space-y-2 overflow-y-auto p-2">
              {columnCards.length === 0 ? (
                <div className="flex min-h-20 items-center justify-center rounded-md border border-dashed border-border-default px-3 text-center text-micro text-fg-subtle">
                  No cards here
                </div>
              ) : (
                columnCards.map((card) => (
                  <PipelineCard
                    key={card.id}
                    card={card}
                    parent={
                      card.parent_run_id
                        ? cardsByRunID.get(card.parent_run_id)
                        : undefined
                    }
                    onChanged={onChanged}
                  />
                ))
              )}
            </div>
          </section>
        );
      })}
    </div>
  );
}

interface CardProps {
  card: PipelineBoardCardDTO;
  parent?: PipelineBoardCardDTO;
  onChanged: () => void;
}

export function PipelineCard({ card, parent, onChanged }: CardProps) {
  const resumeAction = useAsyncAction();
  const depth = Math.min(card.depth, 4);
  const parentLabel =
    parent?.title ||
    (card.parent_run_id ? card.parent_run_id.slice(0, 12) : undefined);
  const attemptCount = card.attempts?.length ?? 0;
  const timestamp = card.updated_at || card.created_at;

  const resumeOperator = async () => {
    const runID = card.run_id;
    if (!runID) return;
    const result = await resumeAction.run(() => resumeRun(runID));
    if (result !== undefined) onChanged();
  };

  return (
    <Card
      className="space-y-2 p-3"
      style={{ marginInlineStart: depth ? `${depth * 10}px` : undefined }}
      data-card-id={card.id}
      data-depth={card.depth}
      role="article"
      aria-label={`${card.title}, ${humanizeToken(card.kind)}`}
    >
      {card.depth > 0 && (
        <div className="flex min-w-0 items-center gap-1 text-caption text-fg-subtle">
          <span aria-hidden>↳</span>
          <span className="shrink-0">Child of</span>
          <span className="truncate font-medium text-fg-muted" title={parentLabel}>
            {parentLabel}
          </span>
          {card.depth > 1 && <Badge variant="neutral">depth {card.depth}</Badge>}
        </div>
      )}

      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="text-sm font-medium leading-snug text-fg-default">
            {card.title}
          </div>
          <div className="mt-0.5 flex flex-wrap items-center gap-1 text-caption text-fg-subtle">
            {card.issue_id && <code title={`Issue ${card.issue_id}`}>#{card.issue_id}</code>}
            {card.workflow_name && <span>{card.workflow_name}</span>}
          </div>
        </div>
        <Badge variant={card.kind === "run" ? "info" : "neutral"}>
          {humanizeToken(card.kind)}
        </Badge>
      </div>

      {card.body && (
        <p className="line-clamp-3 whitespace-pre-wrap text-xs text-fg-muted">
          {card.body}
        </p>
      )}

      <div className="flex flex-wrap items-center gap-1">
        {card.status &&
          (isKnownStatus(card.status) ? (
            <StatusBadge status={card.status} />
          ) : (
            <Badge>{card.status}</Badge>
          ))}
        {card.issue_state && (
          <Badge variant="neutral">{humanizeToken(card.issue_state)}</Badge>
        )}
        {card.priority !== undefined && card.priority !== 0 && (
          <Badge variant="accent">P{card.priority}</Badge>
        )}
        {attemptCount > 0 && (
          <Badge variant="neutral">
            {attemptCount} attempt{attemptCount === 1 ? "" : "s"}
          </Badge>
        )}
        {(card.children_count ?? 0) > 0 && (
          <Badge variant="neutral">
            {card.children_count} child{card.children_count === 1 ? "" : "ren"}
          </Badge>
        )}
      </div>

      {card.labels && card.labels.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {card.labels.map((label) => (
            <Badge key={label} variant="neutral">
              {label}
            </Badge>
          ))}
        </div>
      )}

      {card.error && (
        <InlineBanner tone="danger" layout="inline">
          {card.error}
        </InlineBanner>
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
          <span
            className="ml-auto text-fg-subtle"
            title={timestamp}
          >
            {formatRelative(timestamp)}
          </span>
        )}
      </div>

      {card.status === "paused_operator" && card.run_id && (
        <div className="space-y-2 rounded-md border border-info/40 bg-info-soft p-2">
          <p className="text-micro text-info-fg">
            This run was paused by an operator. Resume it from its persisted checkpoint.
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
            onClick={() => void resumeOperator()}
          >
            Resume run
          </Button>
        </div>
      )}

      {card.status === "paused_waiting_human" && card.run_id && card.node_id && (
        <div className="space-y-2 rounded-md border border-warning/40 bg-warning-soft p-2">
          <div className="text-micro font-medium uppercase tracking-wide text-warning-fg">
            Awaiting input
          </div>
          <HumanPromptForm
            runId={card.run_id}
            nodeId={card.node_id}
            questions={card.questions ?? {}}
            sourceOverride={null}
            onResumed={onChanged}
          />
        </div>
      )}

      {card.status === "paused_waiting_human" && card.run_id && !card.node_id && (
        <InlineBanner tone="warning" layout="inline">
          This legacy pause has no node identifier, so it cannot be answered inline.
          Open the run console to inspect it.
        </InlineBanner>
      )}
    </Card>
  );
}
