import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "wouter";

import { getPipelineBoard } from "@/api/pipelineBoards";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import {
  Button,
  EmptyState,
  InlineBanner,
  Spinner,
} from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";
import { useBotsStore } from "@/store/bots";

import AddTaskDialog from "./AddTaskDialog";
import { PipelineColumns } from "./PipelineColumns";

const POLL_INTERVAL_MS = 3000;

function decodeRouteParam(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

export default function PipelineBoardView() {
  const params = useParams<{ bot: string }>();
  const botID = decodeRouteParam(params.bot ?? "");
  const [addTaskOpen, setAddTaskOpen] = useState(false);
  const bots = useBotsStore((state) => state.bots);
  const fetchBots = useBotsStore((state) => state.fetch);

  useEffect(() => {
    if (bots === null) void fetchBots();
  }, [bots, fetchBots]);

  const query = useQuery({
    queryKey: ["pipeline-board", botID],
    queryFn: ({ signal }) => getPipelineBoard(botID, { signal }),
    enabled: botID.length > 0,
    refetchInterval: POLL_INTERVAL_MS,
    refetchIntervalInBackground: false,
    retry: false,
  });

  const catalogBot = useMemo(
    () => (bots ?? []).find((bot) => bot.name === botID) ?? null,
    [bots, botID],
  );
  const identity = query.data?.board;
  const displayName =
    identity?.display_name || catalogBot?.display_name?.trim() || botID || "Pipeline";

  useHeaderSlot({
    left: (
      <span className="flex min-w-0 items-center gap-1.5 text-xs font-medium text-fg-default">
        <Link href="/pipelines" className="text-fg-muted hover:underline">
          Pipelines
        </Link>
        <span className="text-fg-subtle">/</span>
        <span className="truncate">{displayName}</span>
      </span>
    ),
    right: identity ? (
      <>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => void query.refetch()}
        >
          Refresh
        </Button>
        <Button variant="primary" size="sm" onClick={() => setAddTaskOpen(true)}>
          + Add task
        </Button>
      </>
    ) : null,
  });

  if (!botID) {
    return (
      <EmptyState
        title="Choose a pipeline"
        message="Open a bot-scoped board to inspect its tasks and human interactions."
        action={
          <Link href="/pipelines">
            <Button variant="primary" size="sm">
              Browse pipelines
            </Button>
          </Link>
        }
      />
    );
  }

  if (query.isLoading) {
    return (
      <div className="flex h-full items-center justify-center gap-2 text-sm text-fg-muted">
        <Spinner /> Loading {displayName} pipeline…
      </div>
    );
  }

  if (!query.data) {
    return (
      <div className="flex h-full flex-col p-4">
        <InlineBanner tone="danger" title={`Couldn't load ${displayName}`}>
          <div className="flex flex-wrap items-center gap-3">
            <span>{query.error ? errorMessage(query.error) : "Pipeline board unavailable."}</span>
            <Button variant="secondary" size="sm" onClick={() => void query.refetch()}>
              Retry
            </Button>
            <Link href="/pipelines" className="text-xs text-accent-text hover:underline">
              Back to pipelines
            </Link>
          </div>
        </InlineBanner>
      </div>
    );
  }

  const detail = query.data;

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden">
      <div className="shrink-0 space-y-2 px-4 py-3">
        <div className="flex flex-wrap items-start gap-3">
          <div className="flex min-w-0 flex-1 items-start gap-2">
            {(detail.board.icon || catalogBot?.icon) && (
              <span className="text-xl leading-none" aria-hidden>
                {detail.board.icon || catalogBot?.icon}
              </span>
            )}
            <div className="min-w-0">
              <div className="flex flex-wrap items-baseline gap-2">
                <h1 className="text-display font-semibold text-fg-default">
                  {displayName}
                </h1>
                <code className="text-caption text-fg-subtle">{detail.board.bot_id}</code>
              </div>
              {(detail.board.description || catalogBot?.description) && (
                <p className="mt-0.5 max-w-3xl text-xs text-fg-muted">
                  {detail.board.description || catalogBot?.description}
                </p>
              )}
            </div>
          </div>
          <div className="flex items-center gap-2 text-caption text-fg-subtle">
            <span>
              {detail.cards.length} card{detail.cards.length === 1 ? "" : "s"}
            </span>
            <span>·</span>
            <span>
              {detail.columns.length} column{detail.columns.length === 1 ? "" : "s"}
            </span>
            {detail.generated_at && (
              <>
                <span>·</span>
                <span title={detail.generated_at}>
                  updated {formatRelative(detail.generated_at)}
                </span>
              </>
            )}
          </div>
        </div>

        {!detail.board.enabled && (
          <InlineBanner tone="warning" title="Bot disabled">
            This pipeline remains visible, but new tasks cannot start until the bot is enabled.
          </InlineBanner>
        )}
        {detail.topology_error && (
          <InlineBanner tone="warning" title="Workflow topology incomplete">
            {detail.topology_error}
          </InlineBanner>
        )}
        {query.error && (
          <InlineBanner tone="warning" title="Refresh failed">
            Showing the last successful projection. {errorMessage(query.error)}
          </InlineBanner>
        )}
      </div>

      {detail.columns.length === 0 && detail.cards.length === 0 ? (
        <EmptyState
          title="This pipeline board is empty"
          message="Add a task to feed Todo. Interaction columns appear from the bot workflow topology."
          action={
            <Button variant="primary" size="sm" onClick={() => setAddTaskOpen(true)}>
              Add first task
            </Button>
          }
          className="flex-1"
        />
      ) : (
        <PipelineColumns
          columns={detail.columns}
          cards={detail.cards}
          onChanged={() => void query.refetch()}
        />
      )}

      <AddTaskDialog
        open={addTaskOpen}
        botID={detail.board.bot_id || botID}
        bot={catalogBot}
        botEnabled={detail.board.enabled}
        onOpenChange={setAddTaskOpen}
        onCreated={() => void query.refetch()}
      />
    </div>
  );
}
