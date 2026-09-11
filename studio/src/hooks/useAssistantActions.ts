import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";

import * as runsApi from "@/api/runs";
import { lookupAssistantActions } from "@/api/runs/artifacts";
import type { AssistantActionRequest } from "@/lib/chatDock/assistantActions";

const EMPTY: readonly AssistantActionRequest[] = [];

export function assistantActionsQueryKey(runId: string, revision: number) {
  return ["assistant-actions", runId, revision] as const;
}

export function useAssistantActions(
  runId: string | null,
  revision: number,
): readonly AssistantActionRequest[] {
  const sentInvalidFeedback = useRef(new Set<string>());
  const pendingInvalidFeedback = useRef(new Set<string>());
  const query = useQuery({
    queryKey: assistantActionsQueryKey(runId ?? "", revision),
    queryFn: ({ signal }) =>
      lookupAssistantActions(runId as string, { signal }),
    enabled: !!runId,
    staleTime: 60_000,
    retry: false,
  });

  useEffect(() => {
    const invalid = query.data?.invalid;
    if (!runId || !invalid) return;
    const key = `${runId}:${invalid.sourceKey}:${invalid.attempt}`;
    if (
      sentInvalidFeedback.current.has(key) ||
      pendingInvalidFeedback.current.has(key)
    ) {
      return;
    }
    pendingInvalidFeedback.current.add(key);
    void runsApi
      .deliverHostEvent(runId, "assistant-action-invalid", {
        source: invalid.sourceKey,
        attempt: invalid.attempt,
        terminal: invalid.attempt >= 2,
        rejected: invalid.rejected,
        valid_ids: invalid.validIds,
        canonical_ids: invalid.canonicalIds,
      })
      .then(() => {
        sentInvalidFeedback.current.add(key);
      })
      .catch(() => {
        // A 409 means Copi moved between turns. A later query/remount can
        // retry the same feedback; no action is exposed while it is invalid.
      })
      .finally(() => {
        pendingInvalidFeedback.current.delete(key);
      });
  }, [query.data?.invalid, runId]);

  return query.data?.requests ?? EMPTY;
}
