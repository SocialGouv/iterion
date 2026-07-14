import { CardStackIcon } from "@radix-ui/react-icons";
import { useQuery } from "@tanstack/react-query";
import { useLocation } from "wouter";

import { listPipelineBoards } from "@/api/pipelineBoards";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import {
  Badge,
  Button,
  EmptyState,
  InlineBanner,
  Spinner,
} from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";

export default function PipelineBoardsView() {
  const [, setLocation] = useLocation();
  const query = useQuery({
    queryKey: ["pipeline-boards"],
    queryFn: ({ signal }) => listPipelineBoards({ signal }),
    retry: false,
  });

  useHeaderSlot({
    left: <span className="text-xs font-medium text-fg-default">Pipelines</span>,
    right: (
      <Button
        variant="secondary"
        size="sm"
        loading={query.isFetching}
        onClick={() => void query.refetch()}
      >
        Refresh
      </Button>
    ),
  });

  const boards = [...(query.data ?? [])].sort((a, b) => {
    if (a.board.enabled !== b.board.enabled) return a.board.enabled ? -1 : 1;
    return a.board.display_name.localeCompare(b.board.display_name);
  });

  if (query.isLoading) {
    return (
      <div className="flex h-full items-center justify-center gap-2 text-sm text-fg-muted">
        <Spinner /> Loading pipeline boards…
      </div>
    );
  }

  return (
    <div className="flex h-full flex-col gap-4 overflow-y-auto p-4">
      <div>
        <h1 className="text-display font-semibold text-fg-default">Pipeline boards</h1>
        <p className="mt-1 max-w-2xl text-xs text-fg-muted">
          Each board is scoped to one bot. Its columns are live workflow states and human
          interactions, including child pipelines that need an answer.
        </p>
      </div>

      {query.error && (
        <InlineBanner tone="danger" title="Couldn't load pipeline boards">
          <div className="flex flex-wrap items-center gap-3">
            <span>{errorMessage(query.error)}</span>
            <Button variant="secondary" size="sm" onClick={() => void query.refetch()}>
              Retry
            </Button>
          </div>
        </InlineBanner>
      )}

      {!query.error && boards.length === 0 ? (
        <EmptyState
          title="No pipeline boards yet"
          message="Enable or create a bot first. Every discovered bot gets its own pipeline projection."
          action={
            <Button variant="primary" size="sm" onClick={() => setLocation("/bots")}>
              Browse bots
            </Button>
          }
          className="flex-1"
        />
      ) : (
        <ul className="grid gap-3 [grid-template-columns:repeat(auto-fill,minmax(270px,1fr))]">
          {boards.map((item) => {
            const board = item.board;
            const waiting = item.awaiting_input_count ?? 0;
            return (
              <li key={board.id || board.bot_id}>
                <button
                  type="button"
                  onClick={() =>
                    setLocation(`/pipelines/${encodeURIComponent(board.bot_id)}`)
                  }
                  className="h-full w-full rounded-[var(--radius-lg)] text-left focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
                  title={`Open ${board.display_name} pipeline board`}
                >
                  <div className="h-full">
                    <div className="h-full rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-4 shadow-[var(--shadow-sm)] transition-[box-shadow,transform,border-color] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:-translate-y-0.5 hover:border-border-strong hover:shadow-[var(--shadow-md)]">
                      <div className="flex items-start gap-2">
                        <span
                          className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-surface-2 text-lg"
                          aria-hidden
                        >
                          {board.icon || <CardStackIcon className="h-4 w-4" />}
                        </span>
                        <div className="min-w-0 flex-1">
                          <div className="flex items-center gap-2">
                            <span className="truncate text-sm font-medium text-fg-default">
                              {board.display_name}
                            </span>
                            <Badge variant={board.enabled ? "success" : "neutral"}>
                              {board.enabled ? "Enabled" : "Disabled"}
                            </Badge>
                          </div>
                          <code className="block truncate text-caption text-fg-subtle">
                            {board.bot_id}
                          </code>
                        </div>
                      </div>

                      {board.description ? (
                        <p className="mt-3 line-clamp-2 text-xs text-fg-muted">
                          {board.description}
                        </p>
                      ) : (
                        <p className="mt-3 text-xs italic text-fg-subtle">
                          No bot description.
                        </p>
                      )}

                      <div className="mt-4 flex flex-wrap items-center gap-1">
                        {item.card_count !== undefined && (
                          <Badge variant="neutral">
                            {item.card_count} card{item.card_count === 1 ? "" : "s"}
                          </Badge>
                        )}
                        {item.column_count !== undefined && (
                          <Badge variant="neutral">
                            {item.column_count} column{item.column_count === 1 ? "" : "s"}
                          </Badge>
                        )}
                        {waiting > 0 && (
                          <Badge variant="warning">
                            {waiting} waiting for input
                          </Badge>
                        )}
                        {item.topology_error && (
                          <Badge variant="warning" title={item.topology_error}>
                            Topology warning
                          </Badge>
                        )}
                      </div>
                    </div>
                  </div>
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
