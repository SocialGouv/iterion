import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import { useParams } from "wouter";
import { Group, Panel } from "react-resizable-panels";
import { useQuery } from "@tanstack/react-query";

import { getRunWorkflow, type ExecutionState } from "@/api/runs";
import { useRunStore } from "@/store/run";
import { useUIStore } from "@/store/ui";
import { useRunChildren } from "@/hooks/useRunChildren";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import {
  groupChildrenByNode,
  isSettledRunStatus,
  subbotContinuation,
} from "@/lib/subRuns";
import { useLayoutPersistence } from "@/hooks/useLayoutPersistence";
import { useRunToasts } from "@/hooks/useRunToasts";
import { useRunKeyboard } from "@/hooks/useRunKeyboard";
import {
  readBooleanFlag,
  readJSONFlag,
  writeBooleanFlag,
  writeJSONFlag,
  writeStringFlag,
} from "@/lib/localStorageFlag";

import FileDiffDialog from "./FileDiffDialog";
import FileEditDialog from "./FileEditDialog";
import FloatingChatPanel from "./FloatingChatPanel";
import OperatorPauseBanner from "./OperatorPauseBanner";
import LeftPanel, {
  clampLeftWidth,
  LEFT_COLLAPSED_KEY,
  LEFT_WIDTH_DEFAULT,
  LEFT_WIDTH_KEY,
} from "./LeftPanel";
import NodeDetailPanel from "./NodeDetailPanel";
import QueuedBanner from "./QueuedBanner";
import RunCanvasIR, { defaultIterationFor } from "./RunCanvasIR";
import RunHeader from "./RunHeader";
import SubRunCanvas from "./SubRunCanvas";
import SubRunTabs, { MAIN_FLOW_TAB } from "./SubRunTabs";
import { BottomTabPanel } from "./runView/BottomTabPanel";
import { ExpandStrip, ResizeSeparator } from "./runView/PanelChrome";
import { SideDock } from "./runView/SideDock";
import { RunMetricsBar } from "./runView/RunMetricsBar";
import { RunViewLoadError, RunViewSkeleton } from "./runView/RunViewLoadStates";
import {
  BOTTOM_TAB_KEY,
  EVENTLOG_COLLAPSED_KEY,
  type BottomTab,
} from "./runView/layoutFlags";
import { useDisplayedRunData } from "./runView/useDisplayedRunData";
import { useFileDialogs } from "./runView/useFileDialogs";
import { useFollowLiveNode } from "./runView/useFollowLiveNode";
import { useHorizontalLayout } from "./runView/useHorizontalLayout";
import { useRunConsoleLayout } from "./runView/useRunConsoleLayout";
import { useRunSnapshot } from "./runView/useRunSnapshot";
import { useSelectionState } from "./runView/useSelectionState";

// Poll cadence for the children query while the parent (or any child)
// is still active — subbot children are spawned mid-run, so the run
// console must notice them appearing without a reload.
const SUBRUN_POLL_MS = 3000;

interface RunViewProps {
  // Passed by RunTabHost when this view is hosted in a tab subtree.
  // Falls back to wouter's :id param so legacy `/runs/:id` deep links
  // still work when no host is in scope (e.g. the LaunchView preview).
  runId?: string | null;
}

export default function RunView({ runId: runIdProp }: RunViewProps = {}) {
  const params = useParams<{ id: string }>();
  const runId = runIdProp ?? params.id ?? null;

  const setRunId = useRunStore((s) => s.setRunId);
  const reset = useRunStore((s) => s.reset);
  const snapshot = useRunStore((s) => s.snapshot);
  const events = useRunStore((s) => s.events);
  const executionsById = useRunStore((s) => s.executionsById);
  const wsState = useRunStore((s) => s.wsState);
  const followTail = useRunStore((s) => s.followTail);
  const setFollowTail = useRunStore((s) => s.setFollowTail);
  const browserPreview = useRunStore((s) => s.browser);
  const browserAvailable =
    browserPreview.currentUrl !== null ||
    browserPreview.lastEventSeqSeen !== null ||
    browserPreview.screenshots.length > 0 ||
    browserPreview.liveSession !== null;

  // Selection dials + reset-on-runId. Owns scrubSeq, manualSelectedNodeId,
  // followLiveNode, iterationByNode.
  const {
    scrubSeq,
    setScrubSeq,
    manualSelectedNodeId,
    followLiveNode,
    iterationByNode,
    handleSelectIteration,
    handleSelectNode,
    handleJumpToFailed,
    handleEventSelect,
    handleClearSelection,
    handleToggleFollowLive,
  } = useSelectionState(runId);

  // diffFile / diffMode / editFile dialogs + reset-on-runId.
  const {
    diffFile,
    diffMode,
    editFile,
    handleSelectFile,
    handleEditFile,
    closeDiff,
    closeEdit,
  } = useFileDialogs(runId);

  const verticalLayout = useLayoutPersistence("run-console-v2.vertical", {
    top: 70,
    eventlog: 30,
  });

  // Left panel collapse + width are owned here (not inside LeftPanel) so
  // "Reset layout" can reach them alongside the panel-size layouts, and so
  // the drag-controlled width survives LeftPanel re-mounts. Persisted under
  // the same key LeftPanel used to own.
  const [leftCollapsed, setLeftCollapsed] = useState<boolean>(() =>
    readBooleanFlag(LEFT_COLLAPSED_KEY, false),
  );
  const toggleLeftCollapsed = useCallback(() => {
    setLeftCollapsed((prev) => {
      const next = !prev;
      writeBooleanFlag(LEFT_COLLAPSED_KEY, next);
      return next;
    });
  }, []);

  // Resizable width of the (expanded) left panel — an explicit persisted
  // pixel width + drag handle (see LeftPanel). Not a react-resizable-panels
  // Panel: that library's measure-based initial sizing collapsed a flex-1
  // left panel to its content width.
  const [leftWidth, setLeftWidth] = useState<number>(() =>
    clampLeftWidth(readJSONFlag<number>(LEFT_WIDTH_KEY, LEFT_WIDTH_DEFAULT)),
  );
  const onLeftResize = useCallback((w: number) => {
    const next = clampLeftWidth(w);
    setLeftWidth(next);
    writeJSONFlag(LEFT_WIDTH_KEY, next);
  }, []);

  // Persisted run-console layout/dock dials. The cross-cutting effects
  // below (auto-reveal Browser, "Show event log" token, browserDock →
  // bottomTab redirect) drive the raw setters this hook exposes.
  const {
    browserDock,
    setBrowserDock,
    detailCollapsed,
    toggleDetailCollapsed,
    eventlogCollapsed,
    toggleEventlogCollapsed,
    setEventlogCollapsed,
    bottomTab,
    setBottomTab,
    handleSetBottomTab,
    bottomTabPinned,
    setBottomTabPinned,
    chatDock,
    setChatDock,
    resetLayout,
  } = useRunConsoleLayout();

  // Horizontal layout handle is dock-mode-dependent — see
  // useHorizontalLayout for the picking logic.
  const browserRightDocked = browserDock === "right" && browserAvailable;
  const chatDockedRight = chatDock === "docked-right";
  // Browser + Chat share one tabbed right-hand Side dock (SideDock), so a
  // single "is the side dock open?" bit drives the horizontal layout.
  const sideDockOpen = browserRightDocked || chatDockedRight;
  const horiz = useHorizontalLayout({ sideDockOpen });

  const onResetLayout = () => {
    // Each layout's reset() bumps its own groupKey, remounting the Groups so
    // they re-read the just-reset defaultLayout — see useLayoutPersistence.
    verticalLayout.reset();
    horiz.resetAll();
    resetLayout();
    onLeftResize(LEFT_WIDTH_DEFAULT);
    if (leftCollapsed) toggleLeftCollapsed();
    useUIStore.getState().addToast("Console layout reset", "success");
  };

  // ConversationEmptyState's "Show event log" link bumps a token on
  // the run store; we expand the bottom drawer + flip to "events"
  // when the token changes. The token is a global per-store counter
  // (not reset by reset()), so we baseline against the value the
  // current run-view instance last saw — re-baselined on runId change
  // so navigating between run tabs doesn't replay a stale bump.
  const uiOpenEventLogToken = useRunStore((s) => s.uiOpenEventLogToken);
  const eventLogTokenBaselineRef = useRef(uiOpenEventLogToken);
  useEffect(() => {
    eventLogTokenBaselineRef.current = uiOpenEventLogToken;
    // Only re-baseline on runId change; ignore live token bumps here.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId]);
  useEffect(() => {
    if (uiOpenEventLogToken === eventLogTokenBaselineRef.current) return;
    setBottomTab("events");
    writeStringFlag(BOTTOM_TAB_KEY, "events");
    setBottomTabPinned(true);
    setEventlogCollapsed((prev) => {
      if (!prev) return prev;
      writeBooleanFlag(EVENTLOG_COLLAPSED_KEY, false);
      return false;
    });
  }, [uiOpenEventLogToken, setBottomTab, setBottomTabPinned, setEventlogCollapsed]);

  // Auto-reveal the Browser tab the first time a preview URL becomes
  // available, but only if the user hasn't already pinned a different
  // tab during this view AND the user hasn't moved the pane to the
  // right side.
  useEffect(() => {
    if (
      !bottomTabPinned &&
      browserAvailable &&
      bottomTab !== "browser" &&
      browserDock === "bottom"
    ) {
      setBottomTab("browser");
    }
  }, [browserAvailable, bottomTab, bottomTabPinned, browserDock, setBottomTab]);

  // If the user moves the browser pane to the right side while it was
  // the active bottom tab, redirect them to "logs" so the bottom panel
  // doesn't render an empty/duplicated pane.
  useEffect(() => {
    if (browserDock === "right" && bottomTab === "browser") {
      setBottomTab("logs");
    }
  }, [browserDock, bottomTab, setBottomTab]);

  useEffect(() => {
    setRunId(runId);
    return () => reset();
  }, [runId, setRunId, reset]);

  // Snapshot REST fetch + event-history hydration + retry handling.
  const { loadFailed, handleRetryLoad, refreshSnapshot } = useRunSnapshot(runId);

  // Reset the cross-slice "bottom tab pinned" flag on run change so the
  // previous run's user pin doesn't survive into the new run. Layout/
  // dock preferences persisted to localStorage are intentionally left
  // alone.
  useEffect(() => {
    setBottomTabPinned(false);
  }, [runId, setBottomTabPinned]);

  const wsHandle = useRunWebSocket(runId);
  useRunToasts(events, snapshot?.last_seq);

  // --- Sub-run (subbot child) flow tabs ---------------------------------
  // Children of this run + the parent IR (for grouping children by their
  // spawning subbot node and computing the child banner's continuation
  // targets). The IR is immutable per run → long staleTime; RunCanvasIR
  // keeps its own plain fetch (useWorkflowLoad), so this adds one
  // request, not a second lifecycle.
  const parentWfQuery = useQuery({
    queryKey: ["run-workflow", runId],
    queryFn: () => getRunWorkflow(runId!),
    enabled: !!runId,
    staleTime: 5 * 60_000,
  });
  const parentWf = parentWfQuery.data ?? null;
  const runStatusForPoll = snapshot?.run.status;
  const { data: childRuns } = useRunChildren(runId, true, {
    refetchIntervalMs: SUBRUN_POLL_MS,
    parentActive:
      runStatusForPoll !== undefined && !isSettledRunStatus(runStatusForPoll),
  });

  // Active flow tab: MAIN_FLOW_TAB or a child run id. visitedChildTabs
  // tracks which child canvases have been activated at least once —
  // those stay mounted (hidden) so switching back is instant and the
  // child WS stays warm (same trick as RunsTabsView); never-visited
  // children cost nothing.
  const [flowTab, setFlowTab] = useState<string>(MAIN_FLOW_TAB);
  const [visitedChildTabs, setVisitedChildTabs] = useState<ReadonlySet<string>>(
    () => new Set(),
  );
  useEffect(() => {
    setFlowTab(MAIN_FLOW_TAB);
    setVisitedChildTabs(new Set());
  }, [runId]);
  // Guard: if the active child tab vanished from the children list
  // (store switch, deleted run), fall back to the main flow.
  useEffect(() => {
    if (flowTab === MAIN_FLOW_TAB) return;
    if (!childRuns.some((c) => c.id === flowTab)) setFlowTab(MAIN_FLOW_TAB);
  }, [flowTab, childRuns]);
  const handleSelectFlowTab = useCallback((id: string) => {
    if (id !== MAIN_FLOW_TAB) {
      setVisitedChildTabs((prev) =>
        prev.has(id) ? prev : new Set(prev).add(id),
      );
    }
    setFlowTab(id);
  }, []);

  const childrenByNode = useMemo(
    () => groupChildrenByNode(childRuns, parentWf),
    [childRuns, parentWf],
  );
  // Reverse index for tab tooltips + each child canvas's linkage banner.
  const nodeIdByChildId = useMemo(() => {
    const m = new Map<string, string>();
    for (const [nodeId, list] of childrenByNode) {
      if (!nodeId) continue; // unattributed bucket
      for (const c of list) m.set(c.id, nodeId);
    }
    return m;
  }, [childrenByNode]);

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

  // Live-follow node id with sticky fallback. See useFollowLiveNode.
  const { followLiveNodeId } = useFollowLiveNode({
    runId,
    scrubSeq,
    events,
    executionsById,
    runStatus: snapshot?.run?.status,
  });

  const wfSelectedNodeId =
    followLiveNode && followLiveNodeId ? followLiveNodeId : manualSelectedNodeId;

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

  // Everything the canvas / detail / event log render from the current
  // scrub position (live data when scrubSeq is null) — see
  // useDisplayedRunData. deferredScrubSeq stays internal to the hook.
  const { displayedExecutions, displayedEvents, logClampBytes, runtimeOverrideByNode } =
    useDisplayedRunData(scrubSeq, events, liveExecutions);

  // Workflow view: detail panel reflects the selected node's currently-
  // picked iteration. We expose the full per-node execution list plus
  // the resolved iteration so the panel can render an in-place pill
  // strip and switch which exec drives the tabs without round-tripping
  // through the parent. Sort by `first_seq` (start order) — `loop_iteration`
  // is no longer monotonic post-Option-3 (outer-loop counters can
  // dominate inner ones for many iterations), so sorting on it would
  // scramble the pill order vs. canvas pill order.
  //
  // Note: `selectedIteration` here (and in iterationByNode) is a
  // 0-based array INDEX, not the scalar `loop_iteration` field on
  // ExecutionState. See RunCanvasIR.defaultIterationFor for the
  // motivation (nested loops produce multiple execs with the same
  // `loop_iteration`).
  const selectedNodeExecutions = useMemo(() => {
    if (!wfSelectedNodeId) return [] as ExecutionState[];
    return displayedExecutions
      .filter((e) => e.ir_node_id === wfSelectedNodeId)
      .slice()
      .sort((a, b) => a.first_seq - b.first_seq);
  }, [wfSelectedNodeId, displayedExecutions]);

  const selectedNodeIteration = useMemo(() => {
    if (!wfSelectedNodeId || selectedNodeExecutions.length === 0) return 0;
    return (
      iterationByNode.get(wfSelectedNodeId) ??
      defaultIterationFor(selectedNodeExecutions)
    );
  }, [wfSelectedNodeId, iterationByNode, selectedNodeExecutions]);

  // Kept as a local resolved value for the EventLog filter + queued
  // banner predicate below. Mirrors the resolution the detail panel
  // does internally. Clamps so the panel stays useful when the index
  // points past the current array length (e.g. transient race
  // during a fan-in).
  const detailExec = useMemo(() => {
    if (selectedNodeExecutions.length === 0) return null;
    const i = Math.min(
      Math.max(selectedNodeIteration, 0),
      selectedNodeExecutions.length - 1,
    );
    return selectedNodeExecutions[i] ?? null;
  }, [selectedNodeExecutions, selectedNodeIteration]);

  if (!runId) {
    return (
      <div className="h-full w-full flex flex-col items-center justify-center gap-3 p-8 text-center">
        <h2 className="text-base font-semibold text-fg-default">No run selected</h2>
        <p className="text-xs text-fg-muted max-w-sm">
          Pick a run from the list, or launch a workflow from the editor to create
          one.
        </p>
      </div>
    );
  }
  if (!snapshot) {
    if (loadFailed) {
      return (
        <RunViewLoadError
          runId={runId}
          status={loadFailed.status}
          message={loadFailed.message}
          onRetry={handleRetryLoad}
        />
      );
    }
    return <RunViewSkeleton />;
  }

  const active = snapshot.run.status === "running";
  // The inline Monaco file-editor writes back to the run's on-disk
  // worktree; that endpoint 409s when the worktree is gone (a cloud run's
  // worktree lives on the runner pod; a finalized/gc'd local run's is torn
  // down). Gate every editor entry point on the server-computed flag so an
  // Edit affordance is simply absent rather than error-on-click. When
  // unavailable the file rows still open the read-only diff dialog.
  const canEditFiles = snapshot.run.worktree_available === true;
  const onEditFile = canEditFiles ? handleEditFile : undefined;
  // Drive the EventLog filter from the canvas/detail selection so a
  // click on a node implicitly narrows the event stream.
  const eventLogSelection = detailExec?.execution_id ?? null;
  const liveSeq = snapshot.last_seq;
  const scrubbing = scrubSeq !== null;

  // Pre-pickup state: the run sits on the NATS queue. RunMetrics and
  // Scrubber render nothing useful (no events yet, no budget consumed),
  // so we swap them for the QueuedBanner that surfaces position and
  // exposes a cancel button. The IR canvas stays mounted underneath so
  // the workflow shape is still visible. See cloud-ready plan §F (T-15).
  const isQueued = snapshot.run.status === "queued";
  const isTerminal =
    snapshot.run.status === "finished" ||
    snapshot.run.status === "failed" ||
    snapshot.run.status === "cancelled";
  // The chat input is hidden when the run reached a terminal status,
  // but the transcript stays readable in the floating / docked panel.
  const chatInputDisabled = isQueued || isTerminal;

  // The center column: the canvas + node-detail top split over the bottom
  // drawer, with the optional right-hand Side dock. Extracted to a const to
  // keep the already-deep return readable — it's a flex sibling of the
  // left panel, sized to fill the remaining width.
  const centerColumn = (
    <>
      <Group
        orientation="vertical"
        className="flex-1 min-h-0"
        key={verticalLayout.groupKey}
        defaultLayout={verticalLayout.layout}
        onLayoutChanged={verticalLayout.onChange}
      >
        <Panel id="top" defaultSize={70} minSize={30} className="min-h-0">
          <Group
            orientation="horizontal"
            className="h-full w-full"
            // horiz.active switches instance per dock mode and its
            // groupKey carries the reset nonce, so the Group remounts both
            // on a dock toggle (clean flexGrow redistribution) and on reset.
            key={horiz.active.groupKey}
            defaultLayout={horiz.active.layout}
            onLayoutChanged={horiz.active.onChange}
          >
            <Panel
              id="canvas"
              defaultSize={horiz.canvasSize}
              minSize={25}
              className="min-h-0"
            >
              <div className="h-full w-full flex flex-col">
                {childRuns.length > 0 && (
                  <SubRunTabs
                    mainLabel={snapshot.run.name || snapshot.run.workflow_name}
                    childRuns={childRuns}
                    nodeIdByChildId={nodeIdByChildId}
                    active={flowTab}
                    onSelect={handleSelectFlowTab}
                  />
                )}
                <div className="flex-1 min-h-0 relative">
                  {/* Main flow stays permanently mounted; the scrub
                      saturate wrapper is main-only (child canvases have
                      no scrubber). */}
                  <div
                    className={`absolute inset-0 ${
                      flowTab === MAIN_FLOW_TAB ? "block" : "hidden"
                    } ${scrubbing ? "saturate-50" : ""}`}
                    aria-hidden={flowTab === MAIN_FLOW_TAB ? undefined : true}
                  >
                    <RunCanvasIR
                      runId={runId}
                      executions={displayedExecutions}
                      selectedNodeId={wfSelectedNodeId}
                      onSelectNode={handleSelectNode}
                      iterationByNode={iterationByNode}
                      onSelectIteration={handleSelectIteration}
                      runtimeOverrideByNode={runtimeOverrideByNode}
                      followLive={followLiveNode}
                      onToggleFollowLive={handleToggleFollowLive}
                      subRunsByNode={childrenByNode}
                      onOpenChildRun={handleSelectFlowTab}
                    />
                  </div>
                  {/* Child canvases mount on first activation, then stay
                      mounted hidden (same trick as RunsTabsView) so the
                      child WS stays warm and switching back is instant. */}
                  {childRuns
                    .filter((c) => visitedChildTabs.has(c.id))
                    .map((c) => {
                      const visible = flowTab === c.id;
                      const spawnedBy = nodeIdByChildId.get(c.id) ?? "";
                      return (
                        <div
                          key={c.id}
                          className={`absolute inset-0 ${
                            visible ? "block" : "hidden"
                          }`}
                          aria-hidden={visible ? undefined : true}
                        >
                          <SubRunCanvas
                            runId={c.id}
                            parentRunName={
                              snapshot.run.name || snapshot.run.workflow_name
                            }
                            parentNodeId={spawnedBy}
                            successors={
                              spawnedBy
                                ? subbotContinuation(parentWf, spawnedBy)
                                    .successors
                                : []
                            }
                            onBackToMain={() => setFlowTab(MAIN_FLOW_TAB)}
                          />
                        </div>
                      );
                    })}
                </div>
              </div>
            </Panel>
            {!detailCollapsed && (
              <>
                <ResizeSeparator orientation="horizontal" />
                <Panel
                  id="detail"
                  defaultSize={horiz.detailSize}
                  minSize={18}
                  className="min-h-0"
                >
                  <div className="h-full border-l border-border-default min-h-0 overflow-hidden animate-fade-in-opacity">
                    <NodeDetailPanel
                      runId={runId}
                      runStatus={snapshot.run.status}
                      filePath={snapshot.run.file_path}
                      executions={selectedNodeExecutions}
                      selectedIteration={selectedNodeIteration}
                      onSelectIteration={handleSelectIteration}
                      events={displayedEvents}
                      followLive={followLiveNode}
                      onToggleFollowLive={handleToggleFollowLive}
                      subscribeLogs={wsHandle.subscribeLogs}
                      unsubscribeLogs={wsHandle.unsubscribeLogs}
                      onCollapse={toggleDetailCollapsed}
                      logClampBytes={logClampBytes}
                    />
                  </div>
                </Panel>
              </>
            )}
            {sideDockOpen && (
              <>
                <ResizeSeparator orientation="horizontal" />
                <Panel
                  id="side"
                  defaultSize={horiz.sideSize}
                  minSize={18}
                  className="min-h-0"
                >
                  <SideDock
                    runId={runId}
                    chatDockedRight={chatDockedRight}
                    browserRightDocked={browserRightDocked}
                    scrubSeq={scrubSeq}
                    browserDock={browserDock}
                    onBrowserDockChange={setBrowserDock}
                    chatInputDisabled={chatInputDisabled}
                    onUndockChat={() => setChatDock("floating")}
                    onCloseChat={() => setChatDock("closed")}
                  />
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
              <BottomTabPanel
                runId={runId}
                bottomTab={bottomTab}
                onSelectTab={(t: BottomTab) => handleSetBottomTab(t)}
                browserAvailable={browserAvailable}
                browserRightDocked={browserRightDocked}
                browserDock={browserDock}
                setBrowserDock={setBrowserDock}
                scrubSeq={scrubSeq}
                scrubbing={scrubbing}
                followTail={followTail}
                setFollowTail={setFollowTail}
                displayedEvents={displayedEvents}
                eventLogSelection={eventLogSelection}
                onEventSelect={handleEventSelect}
                onClearSelection={handleClearSelection}
                onCollapse={toggleEventlogCollapsed}
                onSelectNode={handleSelectNode}
                subscribeLogs={wsHandle.subscribeLogs}
                unsubscribeLogs={wsHandle.unsubscribeLogs}
                logClampBytes={logClampBytes}
              />
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
    </>
  );

  return (
    <ReactFlowProvider>
      <div className="h-full w-full overflow-hidden flex flex-col">
        {/* Unified header block: the identity/actions rows + the live-
            vitals ribbon (+ scrubber) read as one bordered unit. Both
            children render `bare` so this container owns the single
            bottom border instead of each painting its own. */}
        <div className="shrink-0 border-b border-border-default">
          <RunHeader
            run={snapshot.run}
            active={active}
            wsState={wsState}
            onResetLayout={onResetLayout}
            bare
          />
          {isQueued ? (
            <QueuedBanner run={snapshot.run} />
          ) : (
            <>
              <RunMetricsBar
                active={active}
                events={events}
                liveSeq={liveSeq}
                scrubSeq={scrubSeq}
                onScrubChange={setScrubSeq}
                onJumpToFailed={handleJumpToFailed}
                bare
              />
              {snapshot.run.status === "paused_operator" && (
                <OperatorPauseBanner run={snapshot.run} />
              )}
            </>
          )}
        </div>
      <div className="flex-1 min-h-0 flex">
        <LeftPanel
          runId={runId}
          run={snapshot.run}
          collapsed={leftCollapsed}
          onToggleCollapsed={toggleLeftCollapsed}
          width={leftWidth}
          onResize={onLeftResize}
          onSelectFile={handleSelectFile}
          onEditFile={onEditFile}
          onMergeComplete={refreshSnapshot}
          onJumpToFailed={handleJumpToFailed}
        />
        <div className="flex-1 min-h-0 flex flex-col">{centerColumn}</div>
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
          mode={diffMode}
          onClose={closeDiff}
          onEdit={onEditFile}
        />
        <FileEditDialog
          runId={runId}
          path={editFile}
          onClose={closeEdit}
        />
        <FloatingChatPanel
          runId={runId}
          dock={chatDock}
          onDockChange={setChatDock}
          inputDisabled={chatInputDisabled}
        />
      </div>
    </ReactFlowProvider>
  );
}
