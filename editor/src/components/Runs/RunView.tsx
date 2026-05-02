import { useCallback, useEffect, useMemo, useState } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import { useParams } from "wouter";
import { Group, Panel, Separator } from "react-resizable-panels";
import { ChevronLeftIcon, ChevronUpIcon } from "@radix-ui/react-icons";

import { getRun, type RunFile } from "@/api/runs";
import { IconButton, Skeleton, Tabs } from "@/components/ui";
import { selectRunningExecution, useRunStore } from "@/store/run";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import { useLayoutPersistence } from "@/hooks/useLayoutPersistence";
import { useRunToasts } from "@/hooks/useRunToasts";
import { useRunKeyboard } from "@/hooks/useRunKeyboard";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";

import { buildExecutionsAt } from "@/lib/snapshotReducer";

import EventLog from "./EventLog";
import FileDiffDialog from "./FileDiffDialog";
import FilesPanel from "./FilesPanel";
import NodeDetailPanel from "./NodeDetailPanel";
import RunCanvasIR, { defaultIterationFor } from "./RunCanvasIR";
import RunHeader from "./RunHeader";
import RunLogPanel from "./RunLogPanel";
import RunMetrics from "./RunMetrics";
import Scrubber from "./Scrubber";

// Runtime override captured from llm_request events. Keyed by IR node id.
// Each value is the most-recent llm_request payload seen for that node.
interface RuntimeLLMOverride {
  model?: string;
  reasoning_effort?: string;
}

const DETAIL_COLLAPSED_KEY = "run-console-v1.detail-collapsed";
const EVENTLOG_COLLAPSED_KEY = "run-console-v1.eventlog-collapsed";

export default function RunView() {
  const params = useParams<{ id: string }>();
  const runId = params.id ?? null;

  const setRunId = useRunStore((s) => s.setRunId);
  const reset = useRunStore((s) => s.reset);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const snapshot = useRunStore((s) => s.snapshot);
  const events = useRunStore((s) => s.events);
  const executionsById = useRunStore((s) => s.executionsById);
  const wsState = useRunStore((s) => s.wsState);
  const followTail = useRunStore((s) => s.followTail);
  const setFollowTail = useRunStore((s) => s.setFollowTail);
  // Time-travel scrubber: when non-null, the canvas/detail/event log
  // render the run *as it was* at this seq. When null (the default),
  // live data flows through. Lives in component state because it's
  // purely UI-driven; the store remains the source of truth for live.
  const [scrubSeq, setScrubSeq] = useState<number | null>(null);
  // Workflow view selects by IR node id (not execution id). The detail
  // panel is driven by the selected node's currently-picked iteration
  // (per-node, default = "current"). Manual picks live alongside a
  // "follow live" toggle: when the toggle is on, the panel auto-shifts
  // to whatever node is currently running; when the user clicks a node
  // the toggle flips off so their pick stays pinned.
  const [manualSelectedNodeId, setManualSelectedNodeId] = useState<string | null>(null);
  const [followLiveNode, setFollowLiveNode] = useState<boolean>(true);
  // Per-IR-node iteration override. Empty map means "use the default
  // (running > paused > latest)". A user click on a timeline pip sets
  // the entry; we never auto-clear so the user's pick stays sticky.
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

  const handleSelectNode = useCallback((nodeId: string | null) => {
    setManualSelectedNodeId(nodeId);
    if (nodeId !== null) setFollowLiveNode(false);
  }, []);

  const handleJumpToFailed = useCallback((nodeId: string) => {
    handleSelectNode(nodeId);
  }, [handleSelectNode]);

  const handleEventSelect = useCallback(
    (nodeId: string, iteration: number) => {
      handleSelectNode(nodeId);
      setIterationByNode((prev) => {
        const next = new Map(prev);
        next.set(nodeId, iteration);
        return next;
      });
    },
    [handleSelectNode],
  );

  const handleClearSelection = useCallback(() => {
    setManualSelectedNodeId(null);
  }, []);

  const handleToggleFollowLive = useCallback(() => {
    setFollowLiveNode((prev) => {
      const next = !prev;
      // Re-engaging follow live: drop the manual pin so the panel
      // jumps to the currently-running node on the next render.
      if (next) setManualSelectedNodeId(null);
      return next;
    });
  }, []);

  const [diffFile, setDiffFile] = useState<RunFile | null>(null);

  const verticalLayout = useLayoutPersistence("run-console-v1.vertical", {
    top: 70,
    eventlog: 30,
  });
  const horizontalLayout = useLayoutPersistence(
    "run-console-v1.horizontal",
    { canvas: 70, detail: 30 },
  );

  const [detailCollapsed, setDetailCollapsed] = useState<boolean>(() =>
    readBooleanFlag(DETAIL_COLLAPSED_KEY),
  );
  const [eventlogCollapsed, setEventlogCollapsed] = useState<boolean>(() =>
    readBooleanFlag(EVENTLOG_COLLAPSED_KEY),
  );
  const [bottomTab, setBottomTab] = useState<"events" | "logs">("events");
  const toggleDetailCollapsed = useCallback(() => {
    setDetailCollapsed((prev) => {
      const next = !prev;
      writeBooleanFlag(DETAIL_COLLAPSED_KEY, next);
      return next;
    });
  }, []);
  const toggleEventlogCollapsed = useCallback(() => {
    setEventlogCollapsed((prev) => {
      const next = !prev;
      writeBooleanFlag(EVENTLOG_COLLAPSED_KEY, next);
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

  const wsHandle = useRunWebSocket(runId);
  useRunToasts(events);

  const liveExecutions = useMemo(
    () => Array.from(executionsById.values()),
    [executionsById],
  );
  const firstFailedNodeId = useMemo(() => {
    for (const ex of liveExecutions) {
      if (ex.status === "failed") return ex.ir_node_id;
    }
    return null;
  }, [liveExecutions]);

  // When follow-live is on, override the manual pick with the
  // currently-running execution. While scrubbing the timeline we
  // disable the auto-track so the panel reflects the past, not the
  // live tail.
  const runningExec = useMemo(() => {
    if (scrubSeq !== null) return null;
    return selectRunningExecution(executionsById);
  }, [scrubSeq, executionsById]);

  const wfSelectedNodeId =
    followLiveNode && runningExec ? runningExec.ir_node_id : manualSelectedNodeId;

  useRunKeyboard({
    selectedNodeId: wfSelectedNodeId,
    executions: liveExecutions,
    iterationByNode,
    onSelectNode: handleSelectNode,
    onSelectIteration: handleSelectIteration,
    onScrubLive: () => setScrubSeq(null),
    onJumpToFailed: firstFailedNodeId
      ? () => handleSelectNode(firstFailedNodeId)
      : undefined,
  });

  // When scrubbing, derive a virtual snapshot at the chosen seq.
  // Otherwise use the live executions map.
  const displayedExecutions = useMemo(() => {
    if (scrubSeq === null) return liveExecutions;
    return buildExecutionsAt(events, scrubSeq);
  }, [scrubSeq, events, liveExecutions]);

  const displayedEvents = useMemo(() => {
    if (scrubSeq === null) return events;
    return events.filter((e) => e.seq <= scrubSeq);
  }, [scrubSeq, events]);

  // Fold llm_request events into a per-node "what was actually sent to
  // the LLM" map. Latest event wins because seq is monotonic. We use
  // displayedEvents rather than raw events so the time-travel scrubber
  // also rewinds the runtime override.
  const runtimeOverrideByNode = useMemo(() => {
    const m = new Map<string, RuntimeLLMOverride>();
    for (const e of displayedEvents) {
      if (e.type !== "llm_request" || !e.node_id) continue;
      const data = e.data ?? {};
      const override: RuntimeLLMOverride = {};
      if (typeof data.model === "string") override.model = data.model;
      if (typeof data.reasoning_effort === "string")
        override.reasoning_effort = data.reasoning_effort;
      if (override.model || override.reasoning_effort) {
        m.set(e.node_id, override);
      }
    }
    return m;
  }, [displayedEvents]);

  // Workflow view: detail panel reflects the selected node's currently-
  // picked iteration. If the user hasn't picked an iteration, fall back
  // to defaultIterationFor (running > paused > latest). No execution at
  // all → panel stays empty.
  const detailExec = useMemo(() => {
    if (!wfSelectedNodeId) return null;
    const matching = displayedExecutions.filter(
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
  }, [wfSelectedNodeId, iterationByNode, displayedExecutions]);

  if (!runId) {
    return <div className="p-4 text-xs text-fg-subtle">Missing run id.</div>;
  }
  if (!snapshot) {
    return <RunViewSkeleton />;
  }

  const active = snapshot.run.status === "running";
  // Drive the EventLog filter from the canvas/detail selection so a
  // click on a node implicitly narrows the event stream.
  const eventLogSelection = detailExec?.execution_id ?? null;
  const liveSeq = snapshot.last_seq;
  const scrubbing = scrubSeq !== null;

  return (
    <ReactFlowProvider>
      <div className="h-screen w-screen overflow-hidden flex flex-col bg-surface-0 text-fg-default">
        <RunHeader run={snapshot.run} active={active} wsState={wsState} />
        <RunMetrics active={active} onJumpToFailed={handleJumpToFailed} />
        <Scrubber
          events={events}
          liveSeq={liveSeq}
          scrubSeq={scrubSeq}
          onChange={setScrubSeq}
          visible={liveSeq > 0}
        />
      <div className="flex-1 min-h-0 flex">
        <FilesPanel runId={runId} onSelectFile={setDiffFile} />
        <div className="flex-1 min-h-0 flex flex-col">
          <Group
            orientation="vertical"
            className="flex-1 min-h-0"
            defaultLayout={verticalLayout.layout}
            onLayoutChanged={verticalLayout.onChange}
          >
            <Panel id="top" defaultSize={70} minSize={30} className="min-h-0">
              <Group
                orientation="horizontal"
                className="h-full w-full"
                defaultLayout={horizontalLayout.layout}
                onLayoutChanged={horizontalLayout.onChange}
              >
                <Panel id="canvas" defaultSize={70} minSize={30} className="min-h-0">
                  <div className={scrubbing ? "h-full w-full saturate-50" : "h-full w-full"}>
                    <RunCanvasIR
                      runId={runId}
                      executions={displayedExecutions}
                      selectedNodeId={wfSelectedNodeId}
                      onSelectNode={handleSelectNode}
                      iterationByNode={iterationByNode}
                      onSelectIteration={handleSelectIteration}
                      runtimeOverrideByNode={runtimeOverrideByNode}
                    />
                  </div>
                </Panel>
                {!detailCollapsed && (
                  <>
                    <ResizeSeparator orientation="horizontal" />
                    <Panel
                      id="detail"
                      defaultSize={30}
                      minSize={18}
                      className="min-h-0"
                    >
                      <div className="h-full border-l border-border-default min-h-0 overflow-hidden animate-fade-in-opacity">
                        <NodeDetailPanel
                          runId={runId}
                          filePath={snapshot.run.file_path}
                          exec={detailExec}
                          events={displayedEvents}
                          followLive={followLiveNode}
                          onToggleFollowLive={handleToggleFollowLive}
                          onCollapse={toggleDetailCollapsed}
                        />
                      </div>
                    </Panel>
                  </>
                )}
              </Group>
            </Panel>
            {!eventlogCollapsed && (
              <>
                <ResizeSeparator orientation="vertical" />
                <Panel
                  id="eventlog"
                  defaultSize={30}
                  minSize={10}
                  className="min-h-0"
                >
                  <div className="h-full border-t border-border-default min-h-0 overflow-hidden animate-fade-in-opacity flex flex-col bg-surface-1">
                    <Tabs
                      value={bottomTab}
                      onValueChange={(v) => setBottomTab(v as "events" | "logs")}
                      items={[
                        { value: "events", label: "Events" },
                        { value: "logs", label: "Logs" },
                      ]}
                      variant="underline"
                      listClassName="px-3"
                    />
                    <div className="flex-1 min-h-0">
                      {bottomTab === "events" ? (
                        <EventLog
                          events={displayedEvents}
                          selectedExecutionId={eventLogSelection}
                          followTail={followTail && !scrubbing}
                          onToggleFollow={setFollowTail}
                          onSelectNodeIteration={handleEventSelect}
                          onClearSelection={handleClearSelection}
                          onCollapse={toggleEventlogCollapsed}
                        />
                      ) : (
                        <RunLogPanel
                          runId={runId}
                          subscribeLogs={wsHandle.subscribeLogs}
                          unsubscribeLogs={wsHandle.unsubscribeLogs}
                          onCollapse={toggleEventlogCollapsed}
                        />
                      )}
                    </div>
                  </div>
                </Panel>
              </>
            )}
          </Group>
          {eventlogCollapsed && (
            <ExpandStrip
              orientation="bottom"
              label="Show event log"
              onClick={toggleEventlogCollapsed}
            />
          )}
        </div>
        {detailCollapsed && (
          <ExpandStrip
            orientation="right"
            label="Show details panel"
            onClick={toggleDetailCollapsed}
          />
        )}
      </div>
        <FileDiffDialog
          runId={runId}
          file={diffFile}
          onClose={() => setDiffFile(null)}
        />
      </div>
    </ReactFlowProvider>
  );
}

function ExpandStrip({
  orientation,
  label,
  onClick,
}: {
  orientation: "right" | "bottom";
  label: string;
  onClick: () => void;
}) {
  const isRight = orientation === "right";
  const stripClass = isRight
    ? "flex flex-col items-center justify-start border-l w-7 py-2"
    : "flex items-center justify-center border-t h-7";
  return (
    <div
      className={`${stripClass} border-border-default bg-surface-1 shrink-0 animate-fade-in-opacity`}
    >
      <IconButton label={label} size="sm" variant="ghost" onClick={onClick}>
        {isRight ? <ChevronLeftIcon /> : <ChevronUpIcon />}
      </IconButton>
    </div>
  );
}

function ResizeSeparator({
  orientation,
}: {
  orientation: "horizontal" | "vertical";
}) {
  // The Group's orientation defines the layout axis; the visible
  // separator runs perpendicular to it. A horizontal Group lays out
  // panels left-to-right, so the separator is a vertical bar (1px
  // wide); a vertical Group stacks top-to-bottom, so it's a horizontal
  // bar (1px tall).
  const isHorizontalGroup = orientation === "horizontal";
  return (
    <Separator
      className={
        isHorizontalGroup
          ? "w-1 bg-border-default/40 hover:bg-accent transition-colors data-[separator-state=drag]:bg-accent"
          : "h-1 bg-border-default/40 hover:bg-accent transition-colors data-[separator-state=drag]:bg-accent"
      }
      aria-label={isHorizontalGroup ? "Resize detail panel" : "Resize event log"}
    />
  );
}

function RunViewSkeleton() {
  return (
    <div className="h-screen w-screen flex flex-col bg-surface-0">
      <div className="border-b border-border-default px-4 py-2 flex items-center gap-3">
        <Skeleton className="h-6 w-16" />
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-5 w-20" />
        <div className="ml-auto">
          <Skeleton className="h-5 w-32" />
        </div>
      </div>
      <div className="flex-1 grid" style={{ gridTemplateColumns: "1fr 360px" }}>
        <div className="p-4">
          <Skeleton className="h-full w-full" />
        </div>
        <div className="p-4 border-l border-border-default space-y-2">
          <Skeleton className="h-5 w-32" />
          <Skeleton className="h-3 w-24" />
          <Skeleton className="h-32 w-full" />
        </div>
      </div>
      <div className="h-32 border-t border-border-default p-2">
        <Skeleton className="h-full w-full" />
      </div>
    </div>
  );
}
