import { Cross2Icon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import type { PipelineBoardCard } from "@/api/pipelineBoards";
import type { UnifiedStatus } from "@/components/Runs/runStatusClasses";
import { Badge, IconButton, InlineBanner, StatusBadge } from "@/components/ui";

import { ProducedElements } from "./ProducedElements";
import { SequentialReviews } from "./SequentialReviews";

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

function StatusChip({ status }: { status?: string }) {
  if (!status) return null;
  return KNOWN_STATUSES.has(status as UnifiedStatus) ? (
    <StatusBadge status={status as UnifiedStatus} />
  ) : (
    <Badge>{status}</Badge>
  );
}

function stringifyValue(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

// InputsList renders the pipeline's full entry input (launch vars / task
// bot-args) as an untruncated key → value list. The sidebar has the room the
// compact card body does not, so values wrap instead of clipping.
function InputsList({ input }: { input?: Record<string, unknown> }) {
  const entries = input ? Object.entries(input) : [];
  if (entries.length === 0) {
    return <p className="text-xs italic text-fg-subtle">No inputs recorded.</p>;
  }
  return (
    <dl className="space-y-2">
      {entries.map(([key, value]) => (
        <div key={key} className="space-y-0.5">
          <dt className="text-micro font-medium uppercase tracking-wide text-fg-muted">
            {key}
          </dt>
          <dd className="whitespace-pre-wrap break-words rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 font-mono text-xs text-fg-default">
            {stringifyValue(value)}
          </dd>
        </div>
      ))}
    </dl>
  );
}

function MetaRow({ card }: { card: PipelineBoardCard }) {
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-caption text-fg-subtle">
      <StatusChip status={card.status} />
      {!!card.priority && card.priority > 0 && (
        <Badge
          variant="warning"
          title={`Priority ${card.priority} — higher numbers launch first from Todo`}
        >
          P{card.priority}
        </Badge>
      )}
      {card.bot_id && <code title={`Bot ${card.bot_id}`}>{card.bot_id}</code>}
      {card.workflow_name && <span className="truncate">{card.workflow_name}</span>}
      {card.issue_id && <code title={`Issue ${card.issue_id}`}>#{card.issue_id}</code>}
      {card.run_id && (
        <Link
          href={`/runs/${encodeURIComponent(card.run_id)}`}
          className="ml-auto font-mono text-accent-text hover:underline"
          title={`Open run ${card.run_id} in the run console`}
        >
          Open run console →
        </Link>
      )}
    </div>
  );
}

function SectionHeading({ children }: { children: React.ReactNode }) {
  return <h3 className="text-xs font-semibold text-fg-default">{children}</h3>;
}

// PipelineCardDetailsBody composes the sidebar sections by lane:
//   - Todo / Draft (no run started)      → Inputs only.
//   - In progress                        → Inputs + Produced elements.
//   - In progress, awaiting human input  → + the response form (first, so the
//                                          operator's action is front and centre).
//   - Done                               → Inputs + Result + Produced elements.
//   - Failed                             → Failure reason + Inputs + Produced
//                                          elements (what it made before dying).
// Exported (separately from the panel wrapper) so it can be unit-tested in
// isolation from the docked-panel chrome.
export function PipelineCardDetailsBody({
  card,
  stale,
  onRefetch,
}: {
  card: PipelineBoardCard;
  // The card vanished from the latest projection (started / finished / folded)
  // — we keep showing the last known snapshot with a heads-up banner.
  stale?: boolean;
  onRefetch: () => void;
}) {
  const reviews = card.pending_reviews ?? [];
  const showProduced =
    !!card.run_id &&
    (card.column_id === "in_progress" ||
      card.column_id === "done" ||
      card.column_id === "failed");
  // The whole pipeline tree — root + sub-bots — so a sub-bot's produced
  // elements surface here too. Falls back to the root run for older
  // projections that don't emit the tree.
  const treeRunIds =
    card.tree_run_ids && card.tree_run_ids.length > 0
      ? card.tree_run_ids
      : card.run_id
        ? [card.run_id]
        : [];

  return (
    <div className="space-y-5">
      {stale && (
        <InlineBanner tone="warning" layout="inline">
          This card is no longer on the board — it may have started, finished,
          or been folded into another pipeline. Showing the last known state.
        </InlineBanner>
      )}

      <MetaRow card={card} />

      {reviews.length > 0 && (
        <section aria-label="Response required" className="space-y-2">
          <SectionHeading>Response required</SectionHeading>
          <SequentialReviews card={card} onResolved={onRefetch} />
        </section>
      )}

      {card.column_id === "failed" && card.error && (
        <section aria-label="Failure" className="space-y-2">
          <SectionHeading>Failure</SectionHeading>
          <InlineBanner tone="danger" layout="inline">
            {card.error}
          </InlineBanner>
        </section>
      )}

      <section aria-label="Inputs" className="space-y-2">
        <SectionHeading>Inputs</SectionHeading>
        <InputsList input={card.entry_input} />
      </section>

      {card.column_id === "done" && card.output && (
        <section aria-label="Result" className="space-y-2">
          <SectionHeading>Result</SectionHeading>
          <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border-default bg-surface-0 p-2 font-mono text-xs text-fg-muted">
            {card.output}
          </pre>
        </section>
      )}

      {showProduced && treeRunIds.length > 0 && (
        <ProducedElements runIds={treeRunIds} status={card.status} />
      )}
    </div>
  );
}

interface Props {
  card: PipelineBoardCard;
  stale?: boolean;
  onClose: () => void;
  onRefetch: () => void;
}

// PipelineCardDetails is the right-hand details panel opened by clicking a
// pipeline card. It docks beside the board (non-modal — the board stays fully
// interactive, and clicking another card just swaps the panel's contents) and
// projects the selected card's inputs, produced elements, and (when the
// pipeline is blocked on a human gate) the response form.
export default function PipelineCardDetails({
  card,
  stale,
  onClose,
  onRefetch,
}: Props) {
  return (
    <aside
      className="flex w-[26rem] max-w-[80vw] shrink-0 flex-col border-l border-border-default bg-surface-1"
      aria-label={`Details for ${card.title}`}
    >
      <div className="flex items-start justify-between gap-2 border-b border-border-default px-4 py-3">
        <div className="min-w-0">
          <h2 className="truncate text-sm font-semibold text-fg-default" title={card.title}>
            {card.title}
          </h2>
          <p className="mt-0.5 truncate text-xs text-fg-subtle">
            {card.kind === "task" ? "Task" : "Run"}
            {card.bot_id ? ` · ${card.bot_id}` : ""}
          </p>
        </div>
        <IconButton label="Close details" size="sm" variant="ghost" onClick={onClose}>
          <Cross2Icon />
        </IconButton>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        <PipelineCardDetailsBody card={card} stale={stale} onRefetch={onRefetch} />
      </div>
    </aside>
  );
}
