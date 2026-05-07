import { useEffect, useState } from "react";

import { getRunWorkflow, type WireSchemaField } from "@/api/runs";

interface State {
  fields: WireSchemaField[] | null; // null while loading or unavailable
  staleHash: boolean;
  error: string | null;
}

const initial: State = { fields: null, staleHash: false, error: null };

// useHumanNodeSchema returns the output_schema fields for the paused
// human node, or null fields when the workflow is unavailable or the
// node has no schema (panel falls back to free-text PauseForm).
export function useHumanNodeSchema(
  runId: string | null,
  nodeId: string | undefined,
): State {
  const [state, setState] = useState<State>(initial);

  useEffect(() => {
    if (!runId || !nodeId) {
      setState(initial);
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const wf = await getRunWorkflow(runId);
        if (cancelled) return;
        const node = wf.nodes.find((n) => n.id === nodeId);
        const fields = node?.output_schema ?? null;
        setState({
          fields,
          staleHash: !!wf.stale_hash,
          error: null,
        });
      } catch (e) {
        if (cancelled) return;
        setState({
          fields: null,
          staleHash: false,
          error: (e as Error).message,
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [runId, nodeId]);

  return state;
}
