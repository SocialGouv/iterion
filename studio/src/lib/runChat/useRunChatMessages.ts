import { useMemo, useRef } from "react";
import { useQuery } from "@tanstack/react-query";

import { getRunWorkflow, type WireWorkflow } from "@/api/runs";
import { useRunStore } from "@/store/run";

import {
  messagesFromEventsCached,
  type MessagesFoldCache,
} from "./messagesFromEvents";
import { irKindResolver, type NodeKindResolver } from "./nodeKindResolver";
import type { RunChatMessage } from "./types";

// useRunChatMessages folds the live event stream into a chat
// transcript driven by the workflow's IR node kinds. The hook is the
// generic counterpart to `useWhatsNextSession` — no bot-specific
// nodeMap, no session lifecycle (the run is already attached by
// RunView, we just derive the messages).
//
// Workflow fetch: `getRunWorkflow` returns the minimal IR projection
// the server exposes (`pkg/runview.WireWorkflow`). Cached by runId
// via TanStack Query so a Conversation ↔ Canvas toggle doesn't refetch.
// While the workflow is loading, the resolver falls back to "banner"
// for every node — events still render with their raw node id as the
// label, which is a better degradation than dropping them.
export function useRunChatMessages(
  runId: string | null,
): RunChatMessage[] {
  const events = useRunStore((s) => s.events);
  // Snapshot is consumed by the fold as a fallback for `node_finished`
  // events missing the embedded output payload. We pass it via a ref
  // (not a useMemo dependency) so the fold doesn't refold on every
  // snapshot heartbeat — the typical event stream carries the output
  // verbatim, so the snapshot path is a rare fallback.
  const snapshot = useRunStore((s) => s.snapshot);
  const snapshotRef = useRef(snapshot);
  snapshotRef.current = snapshot;

  const workflowQuery = useQuery<WireWorkflow>({
    queryKey: ["run-workflow", runId],
    queryFn: () => getRunWorkflow(runId!),
    enabled: !!runId,
    // The IR doesn't change during a run — once fetched, it's stable
    // until the operator launches a new run (and so a new runId).
    staleTime: Infinity,
  });

  // Memoise the resolver so the fold cache key (which uses resolver
  // identity) stays stable across renders. A fresh resolver on every
  // render would force a full refold of the event stream every tick.
  const resolverRef = useRef<{
    workflow: WireWorkflow | null;
    resolver: NodeKindResolver;
  } | null>(null);
  const resolver = useMemo<NodeKindResolver>(() => {
    const wf = workflowQuery.data ?? null;
    if (resolverRef.current && resolverRef.current.workflow === wf) {
      return resolverRef.current.resolver;
    }
    const r = irKindResolver(wf);
    resolverRef.current = { workflow: wf, resolver: r };
    return r;
  }, [workflowQuery.data]);

  // Cache the folded transcript so the next event tick processes only
  // the new tail instead of replaying the whole stream.
  const cacheRef = useRef<MessagesFoldCache | null>(null);
  const messages = useMemo(() => {
    const { messages: out, cache } = messagesFromEventsCached(
      { resolver, events, snapshot: snapshotRef.current },
      cacheRef.current,
    );
    cacheRef.current = cache;
    // Return a fresh array reference so memo consumers see a new
    // value on each event tick (mutating in place wouldn't trigger
    // React re-renders downstream).
    return out.slice();
  }, [resolver, events]);

  return messages;
}
