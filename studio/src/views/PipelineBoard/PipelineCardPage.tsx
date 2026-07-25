import { useQuery } from "@tanstack/react-query";
import { Link, useLocation, useParams } from "wouter";

import { getPipelineBoard } from "@/api/pipelineBoards";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button, EmptyState, InlineBanner, Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";

import { findCardByRouteKey, parseCardRoute } from "./cardRoute";
import PipelineCardDetails from "./PipelineCardDetails";

const POLL_INTERVAL_MS = 3000;

// PipelineCardPage is the GitHub-style dedicated card page at
// /pipelines/cards/:kind/:id — same content as the board drawer, full width.
export default function PipelineCardPage() {
  const params = useParams<{ kind?: string; id?: string }>();
  const [, setLocation] = useLocation();
  const routeKey = parseCardRoute(params.kind, params.id);

  const query = useQuery({
    queryKey: ["pipeline-board"],
    queryFn: ({ signal }) => getPipelineBoard({ signal }),
    refetchInterval: POLL_INTERVAL_MS,
    refetchIntervalInBackground: false,
    retry: false,
  });

  useHeaderSlot({
    left: (
      <div className="flex min-w-0 items-center gap-2 text-xs">
        <Link href="/pipelines" className="text-accent-text hover:underline">
          Pipelines
        </Link>
        <span className="text-fg-subtle">/</span>
        <span className="truncate font-medium text-fg-default">
          {query.data && routeKey
            ? (findCardByRouteKey(query.data.cards, routeKey)?.title ?? "Card")
            : "Card"}
        </span>
      </div>
    ),
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

  if (!routeKey) {
    return (
      <div className="flex h-full flex-col p-4">
        <InlineBanner tone="danger" title="Invalid card link">
          <Link href="/pipelines" className="text-accent-text hover:underline">
            Back to pipelines
          </Link>
        </InlineBanner>
      </div>
    );
  }

  if (query.isLoading) {
    return (
      <div className="flex h-full items-center justify-center gap-2 text-sm text-fg-muted">
        <Spinner /> Loading card…
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

  const card = findCardByRouteKey(query.data.cards, routeKey);
  if (!card) {
    return (
      <div className="flex h-full flex-col p-6">
        <EmptyState
          title="Card not found"
          message="This pipeline ticket is no longer on the board (deleted, or the link is stale)."
          action={
            <Button variant="primary" size="sm" onClick={() => setLocation("/pipelines")}>
              Back to pipelines
            </Button>
          }
        />
      </div>
    );
  }

  return (
    <div className="h-full min-h-0 w-full overflow-hidden">
      <PipelineCardDetails
        card={card}
        presentation="page"
        onClose={() => setLocation("/pipelines")}
        onRefetch={() => void query.refetch()}
      />
    </div>
  );
}
