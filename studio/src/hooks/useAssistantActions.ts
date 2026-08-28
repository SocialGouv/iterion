import { useQuery } from "@tanstack/react-query";

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
  const query = useQuery({
    queryKey: assistantActionsQueryKey(runId ?? "", revision),
    queryFn: ({ signal }) =>
      lookupAssistantActions(runId as string, { signal }),
    enabled: !!runId,
    staleTime: 60_000,
    retry: false,
  });
  return query.data?.requests ?? EMPTY;
}
