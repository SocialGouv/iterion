import { errorMessage } from "@/lib/errorHints";
import { useQuery } from "@tanstack/react-query";

import { getRunWorkflow, type WireSchemaField, type WireWorkflow } from "@/api/runs";

interface State {
  fields: WireSchemaField[] | null;
  loading: boolean;
  staleHash: boolean;
  error: string | null;
}

const initial: State = { fields: null, loading: false, staleHash: false, error: null };

// useHumanNodeSchema returns the output_schema fields for the paused
// human node. Callers MUST distinguish loading=true (don't render the
// form yet) from loading=false && fields===null (no schema, fall back
// to free-text PauseForm). Conflating them ships the fallback during
// the brief fetch window and turns typed answers into strings.
export function useHumanNodeSchema(
  runId: string | null,
  nodeId: string | undefined,
): State {
  const enabled = !!runId && !!nodeId;
  const query = useQuery<WireWorkflow>({
    // Shares the run console's workflow cache (useNodeLabel,
    // useRunChatMessages, RunView). The IR is immutable per run, so a
    // nodeId change re-derives fields from the cached workflow instead
    // of refetching.
    queryKey: ["run-workflow", runId],
    queryFn: () => getRunWorkflow(runId!),
    enabled,
    staleTime: Infinity,
  });

  if (!enabled) return initial;
  // isPending (not isLoading): whenever we have neither data nor a
  // settled error, report loading — never the "no schema" fallback.
  if (query.isPending && !query.error) {
    return { fields: null, loading: true, staleHash: false, error: null };
  }
  if (query.error) {
    return { fields: null, loading: false, staleHash: false, error: errorMessage(query.error) };
  }
  const wf = query.data;
  const node = wf?.nodes.find((n) => n.id === nodeId);
  return {
    fields: node?.output_schema ?? null,
    loading: false,
    staleHash: !!wf?.stale_hash,
    error: null,
  };
}
