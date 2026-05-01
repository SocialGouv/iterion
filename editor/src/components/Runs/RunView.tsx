import { useCallback, useEffect, useMemo, useState } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import { useParams } from "wouter";

import { getRun } from "@/api/runs";
import { useRunStore } from "@/store/run";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";

import EventLog from "./EventLog";
import NodeDetailPanel from "./NodeDetailPanel";
import RunCanvas from "./RunCanvas";
import RunCanvasIR, { defaultIterationFor } from "./RunCanvasIR";
import RunHeader, { type RunViewMode } from "./RunHeader";

export default function RunView() {
  const params = useParams<{ id: string }>();
  const runId = params.id ?? null;

  const setRunId = useRunStore((s) => s.setRunId);
  const reset = useRunStore((s) => s.reset);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const snapshot = useRunStore((s) => s.snapshot);
  const events = useRunStore((s) => s.events);
  const executionsById = useRunStore((s) => s.executionsById);
  const selectedExecutionId = useRunStore((s) => s.selectedExecutionId);
  const setSelectedExecution = useRunStore((s) => s.setSelectedExecution);
  const wsState = useRunStore((s) => s.wsState);
  const followTail = useRunStore((s) => s.followTail);
  const setFollowTail = useRunStore((s) => s.setFollowTail);
  const [viewMode, setViewMode] = useState<RunViewMode>("workflow");
  // Workflow view selects by IR node id (not execution id). The
  // detail panel is driven by the selected node's currently-picked
  // iteration (per-node, default = "current").
  const [wfSelectedNodeId, setWfSelectedNodeId] = useState<string | null>(null);
  // Per-IR-node iteration override. Empty map means "use the default
  // (running > paused > latest)". A user click on a timeline pip
  // sets the entry; we never auto-clear so the user's pick stays
  // sticky across new events.
  const [iterationByNode, setIterationByNode] = useState<Map<string, number>>(
    () => new Map(),
  );
  const handleSelectIteration = useCallback((nodeId: string, iteration: number) => {
    setIterationByNode((prev) => {
      const next = new Map(prev);
      next.set(nodeId, iteration);
      return next;
    });
  }, []);

  useEffect(() => {
    setRunId(runId);
    return () => reset();
  }, [runId, setRunId, reset]);

  // Initial snapshot via REST so the page renders immediately even if
  // the WS is still connecting; the hook's `applySnapshot` on connect
  // will replace it.
  useEffect(() => {
    if (!runId) return;
    let cancelled = false;
    getRun(runId)
      .then((snap) => {
        if (!cancelled) applySnapshot(snap);
      })
      .catch(() => {
        // Surface via the WS error path; REST 404 races are common
        // when navigating immediately after launch.
      });
    return () => {
      cancelled = true;
    };
  }, [runId, applySnapshot]);

  useRunWebSocket(runId);

  // All hooks must run before any early return — pull selectedExec and
  // detailExec resolution up here so the loading/missing-id branches
  // below don't change the hook call order between renders.
  const selectedExec = selectedExecutionId
    ? executionsById.get(selectedExecutionId) ?? null
    : null;
  // Workflow view: detail panel reflects the selected node's
  // currently-picked iteration. If the user hasn't picked an
  // iteration, we fall back to defaultIterationFor (running > paused
  // > latest). No execution at all → panel stays empty.
  const detailExec = useMemo(() => {
    if (viewMode === "execution") return selectedExec;
    if (!wfSelectedNodeId) return null;
    const matching = Array.from(executionsById.values()).filter(
      (e) => e.ir_node_id === wfSelectedNodeId,
    );
    if (matching.length === 0) return null;
    const iter =
      iterationByNode.get(wfSelectedNodeId) ?? defaultIterationFor(matching);
    return (
      matching.find((e) => e.loop_iteration === iter) ??
      matching[matching.length - 1] ??
      null
    );
  }, [viewMode, selectedExec, wfSelectedNodeId, iterationByNode, executionsById]);

  if (!runId) {
    return <div className="p-4 text-xs text-fg-subtle">Missing run id.</div>;
  }
  if (!snapshot) {
    return <div className="p-4 text-xs text-fg-subtle">Loading run…</div>;
  }

  const executions = Array.from(executionsById.values());
  // Server-bound active flag isn't in the per-run snapshot — Phase 1
  // reconciliation guarantees status="running" is genuinely live, so
  // we use it as the signal. The wsState pulse below disambiguates
  // visually when a connection is interrupted.
  const active = snapshot.run.status === "running";

  return (
    <ReactFlowProvider>
      <div className="h-screen w-screen flex flex-col bg-surface-0 text-fg-default">
        <RunHeader
          run={snapshot.run}
          active={active}
          wsState={wsState}
          viewMode={viewMode}
          onViewModeChange={setViewMode}
        />
        <div className="flex-1 grid min-h-0" style={{ gridTemplateColumns: "1fr 360px" }}>
          <div className="relative min-h-0">
            {viewMode === "execution" ? (
              <RunCanvas
                executions={executions}
                events={events}
                selectedExecutionId={selectedExecutionId}
                onSelect={setSelectedExecution}
              />
            ) : (
              <RunCanvasIR
                runId={runId}
                executions={executions}
                selectedNodeId={wfSelectedNodeId}
                onSelectNode={setWfSelectedNodeId}
                iterationByNode={iterationByNode}
                onSelectIteration={handleSelectIteration}
              />
            )}
          </div>
          <div className="border-l border-border-default min-h-0 overflow-hidden">
            <NodeDetailPanel
              runId={runId}
              filePath={snapshot.run.file_path}
              exec={detailExec}
              events={events}
            />
          </div>
        </div>
        <div className="h-48 border-t border-border-default min-h-0">
          <EventLog
            events={events}
            selectedExecutionId={selectedExecutionId}
            followTail={followTail}
            onToggleFollow={setFollowTail}
          />
        </div>
      </div>
    </ReactFlowProvider>
  );
}
