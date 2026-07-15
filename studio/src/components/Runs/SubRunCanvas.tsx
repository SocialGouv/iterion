import { useCallback, useEffect, useMemo, useState } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import { Link } from "wouter";

import { Badge } from "@/components/ui/Badge";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import { createRunStore, RunStoreProvider, useRunStore } from "@/store/run";

import RunCanvasIR from "./RunCanvasIR";
import { STATUS_VARIANT, labelForStatus } from "./runStatusMeta";
import { useDisplayedRunData } from "./runView/useDisplayedRunData";
import { useFollowLiveNode } from "./runView/useFollowLiveNode";
import { useRunSnapshot } from "./runView/useRunSnapshot";

interface Props {
  // The CHILD run to render.
  runId: string;
  // Friendly name of the parent run (banner copy).
  parentRunName: string;
  // IR node id of the parent's subbot node that spawned this child.
  // Empty string when unattributed (legacy child without
  // parent_node_id in a multi-subbot workflow) — the banner then omits
  // the launched-by segment.
  parentNodeId: string;
  // Parent nodes the subbot's output feeds (edges out of the subbot
  // node) — the "output feeds → collect" linkage line.
  successors: string[];
  onBackToMain: () => void;
}

// SubRunCanvas renders a subbot child run's live execution canvas
// inline inside the PARENT run's console (behind a SubRunTabs tab).
// It is a slimmed-down RunView: its own per-run store + WS + snapshot,
// one canvas, no side panels — the "Open full console" link covers
// everything else (including the child's own children; no recursive
// tabs here).
export default function SubRunCanvas(props: Props) {
  // PRIVATE store instance — deliberately NOT the registry store
  // (getOrCreateRunStore): the registry entry is owned by the run-tabs
  // lifecycle (closeTab reset()s + disposes it, counting only run TABS
  // as references), so sharing it from an embedded canvas orphans
  // whichever side outlives the other — and a second useRunWebSocket on
  // a shared store stamps wsState "closed" on unmount under the other
  // consumer's healthy socket. A private store costs at most one extra
  // WS per *visited* child tab, which the broker serves per-subscriber.
  const store = useMemo(() => createRunStore(), [props.runId]);
  return (
    <RunStoreProvider store={store}>
      <SubRunCanvasInner {...props} />
    </RunStoreProvider>
  );
}

function SubRunCanvasInner({
  runId,
  parentRunName,
  parentNodeId,
  successors,
  onBackToMain,
}: Props) {
  const setRunId = useRunStore((s) => s.setRunId);
  const snapshot = useRunStore((s) => s.snapshot);
  const events = useRunStore((s) => s.events);
  const executionsById = useRunStore((s) => s.executionsById);

  // Stamp the private store's runId (createRunStore starts blank). No
  // reset() needed on unmount — the store is exclusively ours and dies
  // with the component.
  useEffect(() => {
    setRunId(runId);
  }, [runId, setRunId]);

  const { loadFailed, handleRetryLoad } = useRunSnapshot(runId);
  useRunWebSocket(runId);

  // Minimal selection dials, mirroring useSelectionState's semantics
  // (node click pins + disables follow; pane click re-engages follow)
  // without the scrub/jump machinery the sub-view doesn't expose.
  const [manualSelectedNodeId, setManualSelectedNodeId] = useState<
    string | null
  >(null);
  const [followLive, setFollowLive] = useState(true);
  const [iterationByNode, setIterationByNode] = useState<Map<string, number>>(
    () => new Map(),
  );
  useEffect(() => {
    setManualSelectedNodeId(null);
    setFollowLive(true);
    setIterationByNode(new Map());
  }, [runId]);

  const liveExecutions = useMemo(
    () => Array.from(executionsById.values()),
    [executionsById],
  );
  // scrubSeq is always null here — the sub-view has no scrubber.
  const { displayedExecutions, runtimeOverrideByNode } = useDisplayedRunData(
    null,
    events,
    liveExecutions,
  );

  const { followLiveNodeId } = useFollowLiveNode({
    runId,
    scrubSeq: null,
    events,
    executionsById,
    runStatus: snapshot?.run.status,
  });

  const handleSelectNode = useCallback((nodeId: string | null) => {
    setManualSelectedNodeId(nodeId);
    setFollowLive(nodeId === null);
  }, []);
  const handleSelectIteration = useCallback(
    (nodeId: string, iteration: number) => {
      setIterationByNode((prev) => {
        const next = new Map(prev);
        next.set(nodeId, iteration);
        return next;
      });
    },
    [],
  );
  const handleToggleFollowLive = useCallback(
    () => setFollowLive((v) => !v),
    [],
  );

  const selectedNodeId =
    followLive && followLiveNodeId ? followLiveNodeId : manualSelectedNodeId;
  const status = snapshot?.run.status;

  return (
    <div className="h-full w-full flex flex-col">
      <div className="shrink-0 flex items-center gap-2 px-2 py-1 border-b border-info/30 bg-info-soft/40 text-caption text-fg-muted min-w-0">
        <button
          type="button"
          onClick={onBackToMain}
          title="Back to the main flow"
          className="shrink-0 px-1.5 py-0.5 rounded border border-border-default bg-surface-1 text-fg-muted hover:bg-surface-2 hover:text-fg-default transition-colors"
        >
          ← Main
        </button>
        <span className="truncate min-w-0">
          ⑂ Sub-run
          {parentNodeId && (
            <>
              {" "}
              · launched by{" "}
              <span className="font-mono text-fg-default">{parentNodeId}</span>
            </>
          )}{" "}
          of <span className="text-fg-default">{parentRunName}</span>
          {successors.length > 0 && (
            <>
              {" "}
              · output feeds →{" "}
              <span className="font-mono text-fg-default">
                {successors.join(", ")}
              </span>
            </>
          )}
        </span>
        {status && (
          <Badge variant={STATUS_VARIANT[status]} className="shrink-0">
            {labelForStatus(status)}
          </Badge>
        )}
        <Link
          href={`/runs/${encodeURIComponent(runId)}`}
          className="ml-auto shrink-0 text-info-fg underline-offset-2 hover:underline"
          title={`Open the full run console for ${runId}`}
        >
          Open full console
        </Link>
      </div>
      <div className="flex-1 min-h-0">
        {!snapshot ? (
          loadFailed ? (
            <div className="h-full p-4 text-xs text-danger-fg flex flex-col items-start gap-2">
              <span>Sub-run unavailable: {loadFailed.message}</span>
              <button
                type="button"
                onClick={handleRetryLoad}
                className="px-2 py-0.5 rounded border border-border-default bg-surface-1 text-fg-muted hover:bg-surface-2 hover:text-fg-default transition-colors"
              >
                Retry
              </button>
            </div>
          ) : (
            <div className="h-full p-4 text-xs text-fg-subtle">
              Loading sub-run…
            </div>
          )
        ) : (
          // RunCanvasIR calls useReactFlow(), so this second mount needs
          // its own ReactFlowProvider (RunView's provider wraps the main
          // canvas instance).
          <ReactFlowProvider>
            <RunCanvasIR
              runId={runId}
              executions={displayedExecutions}
              selectedNodeId={selectedNodeId}
              onSelectNode={handleSelectNode}
              iterationByNode={iterationByNode}
              onSelectIteration={handleSelectIteration}
              runtimeOverrideByNode={runtimeOverrideByNode}
              followLive={followLive}
              onToggleFollowLive={handleToggleFollowLive}
            />
          </ReactFlowProvider>
        )}
      </div>
    </div>
  );
}
