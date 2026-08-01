import { errorMessage } from "@/lib/errorHints";
import { useQuery } from "@tanstack/react-query";

import { getRunWorkflow, type WireSchemaField, type WireWorkflow } from "@/api/runs";

interface State {
  fields: WireSchemaField[] | null;
  // inputFields types the gate's INBOUND payload (the pause's questions
  // map) so it can be rendered above the answer form. Independent of
  // `fields`: a gate commonly declares an output schema and no input
  // schema, in which case the renderer infers from each value's shape.
  inputFields: WireSchemaField[] | null;
  loading: boolean;
  staleHash: boolean;
  error: string | null;
}

export interface HumanNodeSchema extends State {
  // reload re-fetches the run workflow — the "Retry" affordance when the
  // first fetch failed (see HumanPromptForm). Backed by react-query's
  // refetch, so it re-runs the query and repopulates the shared cache.
  reload: () => void;
}

const initial: State = {
  fields: null,
  inputFields: null,
  loading: false,
  staleHash: false,
  error: null,
};

// useHumanNodeSchema returns the output_schema fields for the paused
// human node (plus its input_schema, which types the inbound payload).
// Callers MUST distinguish three outcomes, never conflate them:
//   - loading=true                      → don't render the form yet
//   - loading=false && error!=null      → the fetch FAILED; surface it
//     and offer reload() — do NOT silently fall back (a fallback form
//     that can't express the gate's verdict strands the operator and
//     drops their notes; iterion#244)
//   - loading=false && error==null && fields empty → the node genuinely
//     has no output schema → free-text PauseForm fallback is correct
export function useHumanNodeSchema(
  runId: string | null,
  nodeId: string | undefined,
): HumanNodeSchema {
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

  const reload = () => {
    void query.refetch();
  };

  if (!enabled) return { ...initial, reload };
  // isPending (not isLoading): whenever we have neither data nor a
  // settled error, report loading — never the "no schema" fallback.
  if (query.isPending && !query.error) {
    return { ...initial, loading: true, reload };
  }
  if (query.error) {
    return { ...initial, error: errorMessage(query.error), reload };
  }
  const wf = query.data;
  const node = wf?.nodes.find((n) => n.id === nodeId);
  return {
    fields: node?.output_schema ?? null,
    inputFields: node?.input_schema ?? null,
    loading: false,
    staleHash: !!wf?.stale_hash,
    error: null,
    reload,
  };
}
