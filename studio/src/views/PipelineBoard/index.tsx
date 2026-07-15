import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { getPipelineBoard, type PipelineBoardCard } from "@/api/pipelineBoards";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button, EmptyState, InlineBanner, Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";

import AddTaskDialog from "./AddTaskDialog";
import PipelineCardDetails from "./PipelineCardDetails";
import { PipelineColumns } from "./PipelineColumns";
import { findFollowCard } from "./selection";

const POLL_INTERVAL_MS = 3000;

export default function PipelineBoardView() {
  const [addTaskOpen, setAddTaskOpen] = useState(false);
  const [editTask, setEditTask] = useState<PipelineBoardCard | null>(null);
  // The card whose details sidebar is open. Held as the click-time snapshot;
  // its live version is re-derived from each poll so status / reviews /
  // produced elements stay current while the drawer is open.
  const [selected, setSelected] = useState<PipelineBoardCard | null>(null);

  const query = useQuery({
    queryKey: ["pipeline-board"],
    queryFn: ({ signal }) => getPipelineBoard({ signal }),
    refetchInterval: POLL_INTERVAL_MS,
    refetchIntervalInBackground: false,
    retry: false,
  });

  useHeaderSlot({
    left: <span className="text-xs font-medium text-fg-default">Pipelines</span>,
    right: (
      <>
        <Button
          variant="secondary"
          size="sm"
          loading={query.isFetching}
          onClick={() => void query.refetch()}
        >
          Refresh
        </Button>
        <Button variant="primary" size="sm" onClick={() => setAddTaskOpen(true)}>
          + Add task
        </Button>
      </>
    ),
  });

  if (query.isLoading) {
    return (
      <div className="flex h-full items-center justify-center gap-2 text-sm text-fg-muted">
        <Spinner /> Loading pipeline board…
      </div>
    );
  }

  if (!query.data) {
    return (
      <div className="flex h-full flex-col p-4">
        <InlineBanner tone="danger" title="Couldn't load the pipeline board">
          <div className="flex flex-wrap items-center gap-3">
            <span>
              {query.error ? errorMessage(query.error) : "Pipeline board unavailable."}
            </span>
            <Button variant="secondary" size="sm" onClick={() => void query.refetch()}>
              Retry
            </Button>
          </div>
        </InlineBanner>
      </div>
    );
  }

  const board = query.data;
  const { concurrency } = board;

  // Re-locate the open card in the latest projection (id can change as a task
  // becomes a run); fall back to the snapshot and flag it stale when the card
  // has left the board entirely.
  const liveSelected = selected ? findFollowCard(board.cards, selected) : null;
  const detailCard = liveSelected ?? selected;
  const detailStale = selected !== null && liveSelected === null;

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden">
      <div className="shrink-0 space-y-2 px-4 py-3">
        <div className="flex flex-wrap items-start gap-3">
          <div className="min-w-0 flex-1">
            <h1 className="text-display font-semibold text-fg-default">Pipeline board</h1>
            <p className="mt-0.5 max-w-3xl text-xs text-fg-muted">
              Every launched pipeline (and not-yet-started task) across all bots, bucketed
              into four fixed lanes. Running cards are placed by run state; stage a Draft
              ticket with its “→ Todo” button (or edit it first), and click any card for
              details.
            </p>
          </div>
          <div className="flex items-center gap-2 text-caption text-fg-subtle">
            {concurrency.enabled && (
              <span
                title={`Concurrency cap: ${concurrency.max}`}
                className="rounded-full border border-border-default bg-surface-1 px-2 py-0.5 font-medium text-fg-muted"
              >
                {concurrency.active} running · {concurrency.waiting} waiting
                {concurrency.max > 0 ? ` · max ${concurrency.max}` : ""}
              </span>
            )}
            <span>
              {board.cards.length} card{board.cards.length === 1 ? "" : "s"}
            </span>
            {board.generated_at && (
              <>
                <span>·</span>
                <span title={board.generated_at}>
                  updated {formatRelative(board.generated_at)}
                </span>
              </>
            )}
          </div>
        </div>

        {board.topology_error && (
          <InlineBanner tone="warning" title="Workflow topology incomplete">
            {board.topology_error}
          </InlineBanner>
        )}
        {query.error && (
          <InlineBanner tone="warning" title="Refresh failed">
            Showing the last successful projection. {errorMessage(query.error)}
          </InlineBanner>
        )}
      </div>

      <div className="flex min-h-0 flex-1 overflow-hidden">
        <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
          {board.cards.length === 0 ? (
            <EmptyState
              title="No pipelines yet"
              message="Add a task to feed Todo, or launch a bot. Running pipelines and their human reviews appear here automatically."
              action={
                <Button variant="primary" size="sm" onClick={() => setAddTaskOpen(true)}>
                  Add first task
                </Button>
              }
              className="flex-1"
            />
          ) : (
            <PipelineColumns
              board={board}
              onRefetch={() => void query.refetch()}
              onEditTask={setEditTask}
              onOpenCard={setSelected}
            />
          )}
        </div>

        {detailCard && (
          <PipelineCardDetails
            card={detailCard}
            stale={detailStale}
            onClose={() => setSelected(null)}
            onRefetch={() => void query.refetch()}
          />
        )}
      </div>

      <AddTaskDialog
        open={addTaskOpen}
        onOpenChange={setAddTaskOpen}
        onCreated={() => void query.refetch()}
      />

      <AddTaskDialog
        open={editTask !== null}
        editTask={editTask ?? undefined}
        onOpenChange={(o) => {
          if (!o) setEditTask(null);
        }}
        onCreated={() => {
          setEditTask(null);
          void query.refetch();
        }}
      />
    </div>
  );
}
