import { useEffect, useState } from "react";

import { getRunWorkflow, type WireSchemaField } from "@/api/runs";

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
  const [state, setState] = useState<State>(initial);

  useEffect(() => {
    if (!runId || !nodeId) {
      setState(initial);
      return;
    }
    let cancelled = false;
    setState({ fields: null, loading: true, staleHash: false, error: null });
    (async () => {
      try {
        const wf = await getRunWorkflow(runId);
        if (cancelled) return;
        const node = wf.nodes.find((n) => n.id === nodeId);
        setState({
          fields: node?.output_schema ?? null,
          loading: false,
          staleHash: !!wf.stale_hash,
          error: null,
        });
      } catch (e) {
        if (cancelled) return;
        setState({
          fields: null,
          loading: false,
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
