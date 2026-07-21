import type { PipelineBoardCard, PipelineConcurrency } from "@/api/pipelineBoards";

import { computeQueueSummary } from "./queueSummary";

export function QueueBanner({
  cards,
  concurrency,
  onOpenCard,
}: {
  cards: PipelineBoardCard[];
  concurrency?: PipelineConcurrency;
  onOpenCard?: (card: PipelineBoardCard) => void;
}) {
  const s = computeQueueSummary(cards, concurrency);
  if (s.readyCount === 0 && s.waitingDepsCount === 0 && s.draftCount === 0) {
    return null;
  }

  return (
    <div
      className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border border-border-default bg-surface-1 px-3 py-2 text-xs"
      role="status"
      aria-label="Launch queue summary"
    >
      <span className="font-semibold text-fg-default">Queue</span>
      <span className="text-fg-muted">
        <strong className="font-medium text-success-fg">{s.readyCount}</strong> ready
      </span>
      <span className="text-fg-subtle">·</span>
      <span className="text-fg-muted">
        <strong className="font-medium text-warning-fg">{s.waitingDepsCount}</strong> waiting
        on deps
      </span>
      {s.draftCount > 0 && (
        <>
          <span className="text-fg-subtle">·</span>
          <span className="text-fg-muted">
            <strong className="font-medium text-fg-default">{s.draftCount}</strong> draft
          </span>
        </>
      )}
      {s.slotsFree !== null && s.slotsMax !== null && (
        <>
          <span className="text-fg-subtle">·</span>
          <span
            className="text-fg-muted"
            title={`${s.slotsActive ?? 0} active of ${s.slotsMax} max concurrent roots`}
          >
            slots{" "}
            <strong className="font-medium text-fg-default">
              {s.slotsFree}/{s.slotsMax}
            </strong>{" "}
            free
          </span>
        </>
      )}
      {s.nextUp && (
        <>
          <span className="text-fg-subtle">·</span>
          <button
            type="button"
            onClick={() => onOpenCard?.(s.nextUp!)}
            className="min-w-0 truncate text-left text-accent-text hover:underline"
            title={`Next up: ${s.nextUp.title}`}
          >
            Next up: <span className="font-medium">{s.nextUp.title}</span>
            {(s.nextUp.priority ?? 0) > 0 ? ` (P${s.nextUp.priority})` : ""}
          </button>
        </>
      )}
    </div>
  );
}
