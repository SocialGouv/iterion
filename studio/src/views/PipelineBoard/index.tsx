import { useEffect, useRef, useState } from "react";
import { Link } from "wouter";
import { useQuery } from "@tanstack/react-query";

import { getPipelineBoard, type PipelineBoardCard } from "@/api/pipelineBoards";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button, EmptyState, InlineBanner, Spinner } from "@/components/ui";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";
import { useUIStore } from "@/store/ui";

import AddTaskDialog from "./AddTaskDialog";
import PipelineCardDetails from "./PipelineCardDetails";
import {
  PipelineColumns,
  type OpenCardFocus,
} from "./PipelineColumns";
import {
  collectFilterOptions,
  emptyPipelineFilters,
  filterPipelineCards,
} from "./filters";
import { findFollowCard } from "./selection";

const POLL_INTERVAL_MS = 3000;

export default function PipelineBoardView() {
  const [addTaskOpen, setAddTaskOpen] = useState(false);
  const [editTask, setEditTask] = useState<PipelineBoardCard | null>(null);
  const [selected, setSelected] = useState<PipelineBoardCard | null>(null);
  const [drawerFocus, setDrawerFocus] = useState<OpenCardFocus>("default");
  const [filters, setFilters] = useState(emptyPipelineFilters);
  // Repo-first scoping: default the visible cards to the sidebar's active
  // repo (cloud only, non-overview); the "Include unscoped" toggle lets
  // repo-less cards through alongside. In overview / local mode the filter
  // is inactive — every card renders as before.
  const {
    activeRepo,
    overview,
    enabled: repoScopeEnabled,
  } = useActiveRepo();
  const repoScope =
    repoScopeEnabled && !overview && activeRepo
      ? activeRepo.repo_full_name
      : null;
  const [includeUnscoped, setIncludeUnscoped] = useState(false);
  const addToast = useUIStore((s) => s.addToast);
  // issue_id → run_id snapshot for launch-toast detection.
  const prevIssueRuns = useRef<Map<string, string>>(new Map());
  const launchToastPrimed = useRef(false);

  const query = useQuery({
    queryKey: ["pipeline-board"],
    queryFn: ({ signal }) => getPipelineBoard({ signal }),
    refetchInterval: POLL_INTERVAL_MS,
    refetchIntervalInBackground: false,
    retry: false,
  });

  // Toast when a ticket-backed card gains a new run_id while in progress
  // (admission loop or dispatcher just launched it).
  useEffect(() => {
    const cards = query.data?.cards;
    if (!cards) return;
    const next = new Map<string, string>();
    for (const c of cards) {
      if (c.issue_id && c.run_id) next.set(c.issue_id, c.run_id);
    }
    if (!launchToastPrimed.current) {
      prevIssueRuns.current = next;
      launchToastPrimed.current = true;
      return;
    }
    for (const [issueId, runId] of next) {
      const prev = prevIssueRuns.current.get(issueId);
      if (prev === runId) continue;
      if (prev && prev === runId) continue;
      // New or changed run id for this issue.
      if (!prev || prev !== runId) {
        const card = cards.find((c) => c.issue_id === issueId);
        if (card?.column_id === "in_progress") {
          addToast(
            `Started “${card.title}” · ${runId.slice(0, 12)}…`,
            "success",
            {
              action: {
                label: "Open run",
                onClick: () => {
                  window.location.href = `/runs/${encodeURIComponent(runId)}`;
                },
              },
            },
          );
        }
      }
    }
    prevIssueRuns.current = next;
  }, [query.data?.cards, addToast]);

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

  const liveSelected = selected ? findFollowCard(board.cards, selected) : null;
  const detailCard = liveSelected ?? selected;
  const detailStale = selected !== null && liveSelected === null;

  const filterOptions = collectFilterOptions(board.cards);
  // ONE filtered set: the text/label/kind chips AND the repo scope. The
  // lifecycle chips (Opened/Closed tabs) are applied further down, inside
  // PipelineColumns, so In progress is never hidden by an inventory tab.
  const filteredCards = filterPipelineCards(
    board.cards,
    filters,
    repoScope,
    includeUnscoped,
  );

  const openCard = (card: PipelineBoardCard, focus: OpenCardFocus = "default") => {
    setDrawerFocus(focus);
    setSelected(card);
  };

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden">
      <div className="shrink-0 space-y-2 px-4 py-3">
        <div className="flex flex-wrap items-start gap-3">
          <div className="min-w-0 flex-1">
            {/* The view's title lives in the header slot ("Pipelines");
                the body opens with the explanatory intro line only. */}
            <p className="max-w-3xl text-xs text-fg-muted">
              Live runs up top. Inventory tabs{" "}
              <strong className="font-medium text-fg-default">Opened</strong>{" "}
              (default) and{" "}
              <strong className="font-medium text-fg-default">Closed</strong>.
              Queue banner shows ready / waiting / next up. One primary action
              per card; more in ⋯. Cards advance automatically as their runs
              progress — the only drag here is Opened → In progress, which
              launches a ticket immediately, unlike the{" "}
              <Link href="/board" className="text-accent-text hover:underline">
                Board
              </Link>
              .
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
            {board.generated_at && (
              <span title={board.generated_at}>
                updated {formatRelative(board.generated_at)}
              </span>
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

      <div className="relative min-h-0 flex-1 overflow-hidden">
        <div className="flex h-full min-w-0 flex-col overflow-hidden">
          {board.cards.length === 0 ? (
            <EmptyState
              title="No pipelines yet"
              message="Add a task or launch a bot. Running pipelines and their human reviews appear here automatically."
              action={
                <Button variant="primary" size="sm" onClick={() => setAddTaskOpen(true)}>
                  Add first task
                </Button>
              }
              className="flex-1"
            />
          ) : (
            <PipelineColumns
              board={{ ...board, cards: filteredCards }}
              allCardsForQueue={board.cards}
              onRefetch={() => void query.refetch()}
              onEditTask={setEditTask}
              onOpenCard={openCard}
              filters={filters}
              onFiltersChange={setFilters}
              onFiltersReset={() => setFilters(emptyPipelineFilters())}
              filterOptions={filterOptions}
              repoScope={repoScope}
              includeUnscoped={includeUnscoped}
              onIncludeUnscopedChange={setIncludeUnscoped}
            />
          )}
        </div>

        {detailCard && (
          <PipelineCardDetails
            card={detailCard}
            stale={detailStale}
            presentation="overlay"
            focusSection={drawerFocus}
            onClose={() => {
              setSelected(null);
              setDrawerFocus("default");
            }}
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
