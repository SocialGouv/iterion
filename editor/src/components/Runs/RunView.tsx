import { useCallback, useDeferredValue, useEffect, useMemo, useState } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import { useParams } from "wouter";
import { Group, Panel, Separator } from "react-resizable-panels";
import { ChevronLeftIcon, ChevronUpIcon } from "@radix-ui/react-icons";

import {
  getRun,
  type ExecutionState,
  type RunFile,
  type RunFilesMode,
} from "@/api/runs";
import { IconButton, Skeleton, Tabs } from "@/components/ui";
import { selectRunningExecution, useRunStore } from "@/store/run";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import { useLayoutPersistence } from "@/hooks/useLayoutPersistence";
import { useRunToasts } from "@/hooks/useRunToasts";
import { useRunKeyboard } from "@/hooks/useRunKeyboard";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";

import { buildExecutionsAt } from "@/lib/snapshotReducer";

import BrowserPane, { type BrowserDock } from "./BrowserPane";
import EventLog from "./EventLog";
import FileDiffDialog from "./FileDiffDialog";
import HumanInteractionPanel from "./HumanInteractionPanel";
import LeftPanel from "./LeftPanel";
import NodeDetailPanel from "./NodeDetailPanel";
import QueuedBanner from "./QueuedBanner";
import RunCanvasIR, { defaultIterationFor } from "./RunCanvasIR";
import RunHeader from "./RunHeader";
import RunLogPanel from "./RunLogPanel";
import RunMetrics from "./RunMetrics";
import ReportTab from "./ReportTab";
import Scrubber from "./Scrubber";

import { readNodeOutputMeta, type DelegateOutputMeta } from "@/lib/delegateMeta";

// `RuntimeLLMOverride` aliases the cross-file `DelegateOutputMeta`
// shape — kept under this name locally so its run-view role (override
// of the .iter-declared model/effort for the canvas "live" badge) is
// obvious at call sites.
type RuntimeLLMOverride = DelegateOutputMeta;

const DETAIL_COLLAPSED_KEY = "run-console-v1.detail-collapsed";
const EVENTLOG_COLLAPSED_KEY = "run-console-v1.eventlog-collapsed";
const BROWSER_DOCK_KEY = "run-console-v1.browser-dock";

function readBrowserDock(): BrowserDock {
  try {
    const raw = window.localStorage.getItem(BROWSER_DOCK_KEY);
    return raw === "right" ? "right" : "bottom";
  } catch {
    return "bottom";
  }
}

function writeBrowserDock(dock: BrowserDock): void {
  try {
    window.localStorage.setItem(BROWSER_DOCK_KEY, dock);
  } catch {
    // storage may be unavailable
  }
}

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
  const browserPreview = useRunStore((s) => s.browser);
  const browserAvailable =
    browserPreview.currentUrl !== null ||
    browserPreview.lastEventSeqSeen !== null ||
    browserPreview.screenshots.length > 0 ||
    browserPreview.liveSession !== null;
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
  // Mode the FilesPanel was in when the user clicked the row; forwarded
  // to FileDiffDialog so it requests the same range from the backend.
  const [diffMode, setDiffMode] = useState<RunFilesMode>("");
  const handleSelectFile = useCallback(
    (file: RunFile, mode: RunFilesMode) => {
      setDiffMode(mode);
      setDiffFile(file);
    },
    [],
  );

  const verticalLayout = useLayoutPersistence("run-console-v1.vertical", {
    top: 70,
    eventlog: 30,
  });
  const horizontalLayout = useLayoutPersistence(
    "run-console-v1.horizontal",
    { canvas: 70, detail: 30 },
  );
  // Separate layout key so the right-dock split (canvas / detail /
  // browser) doesn't collide with the canvas/detail-only layout when
  // the user toggles the dock.
  const horizontalLayoutWithBrowser = useLayoutPersistence(
    "run-console-v1.horizontal-with-browser",
    { canvas: 50, detail: 25, browserRight: 25 },
  );

  const [browserDock, setBrowserDockState] = useState<BrowserDock>(() =>
    readBrowserDock(),
  );
  const setBrowserDock = useCallback((next: BrowserDock) => {
    setBrowserDockState(next);
    writeBrowserDock(next);
  }, []);

  const [detailCollapsed, setDetailCollapsed] = useState<boolean>(() =>
    readBooleanFlag(DETAIL_COLLAPSED_KEY),
  );
  const [eventlogCollapsed, setEventlogCollapsed] = useState<boolean>(() =>
    readBooleanFlag(EVENTLOG_COLLAPSED_KEY),
  );
  const [bottomTab, setBottomTab] = useState<
    "events" | "logs" | "report" | "browser"
  >("logs");
  // Tracks whether the user has manually changed the bottom tab during
  // this run view, so we don't yank the tab back to "browser" on every
  // new preview_url event after they explicitly picked another panel.
  const [bottomTabPinned, setBottomTabPinned] = useState<boolean>(false);
  const handleSetBottomTab = useCallback(
    (tab: "events" | "logs" | "report" | "browser") => {
      setBottomTab(tab);
      setBottomTabPinned(true);
    },
    [],
  );
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
  }, [browserAvailable, bottomTab, bottomTabPinned, browserDock]);

  // If the user moves the browser pane to the right side while it was
  // the active bottom tab, redirect them to "logs" so the bottom panel
  // doesn't render an empty/duplicated pane.
  useEffect(() => {
    if (browserDock === "right" && bottomTab === "browser") {
      setBottomTab("logs");
    }
  }, [browserDock, bottomTab]);

  useEffect(() => {
    setRunId(runId);
    return () => reset();
  }, [runId, setRunId, reset]);

  // Initial snapshot via REST so the page renders immediately even if
  // the WS is still connecting; the hook's `applySnapshot` on connect
  // will replace it.
  //
  // Retry on 404: the launch API returns the run_id as soon as the
  // engine goroutine is scheduled, but the goroutine still needs a
  // beat to call store.CreateRun before run.json exists on disk.
  // Fetching too early therefore 404s, and without a retry the page
  // gets stuck in <RunViewSkeleton/> until the user reloads — the
  // WS path was supposed to fill the gap but doesn't always push the
  // initial snapshot eagerly. A short backoff loop closes the race
  // for the common case (run.json typically lands within ~50–200ms)
  // without papering over a genuinely missing run.
  useEffect(() => {
    if (!runId) return;
    let cancelled = false;
    let attempt = 0;
    const fetchWithRetry = () => {
      getRun(runId)
        .then((snap) => {
          if (!cancelled) applySnapshot(snap);
        })
        .catch(() => {
          if (cancelled) return;
          attempt += 1;
          // ~5s budget total: 250ms × 20 = 5000ms, more than enough
          // for the local launch → CreateRun race.
          if (attempt < 20) {
            setTimeout(fetchWithRetry, 250);
          }
        });
    };
    fetchWithRetry();
    return () => {
      cancelled = true;
    };
  }, [runId, applySnapshot]);

  // refreshSnapshot — used by post-merge UI to refetch run.json so
  // RunHeader and the merge-state-driven UI catch up after a Commits-
  // tab merge action lands. The WS pushes events but not run-meta
  // updates, so a manual REST fetch is the simplest path.
  const refreshSnapshot = useCallback(() => {
    if (!runId) return;
    getRun(runId)
      .then(applySnapshot)
      .catch(() => undefined);
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

  // Sticky follow-live cache: bridges the transient gap between
  // `node_finished` (previous exec flips to finished) and `node_started`
  // (next exec arrives). Those events are emitted by the engine across
  // separate WS messages — with a `SaveCheckpoint` disk I/O + edge
  // selection in between — so the client sees a brief window where no
  // execution carries status="running" even though the run is still
  // active and producing logs.
  //
  // Without this cache, `runningExec` flips to null during the window,
  // `wfSelectedNodeId` collapses to `manualSelectedNodeId` (null when
  // follow-live is engaged), and the detail panel + per-node log
  // filter both blank out until the next node_started lands. With it,
  // we hold the last known running node id so the UI stays anchored
  // through the gap and only updates once the new running exec
  // materialises.
  const [lastRunningNodeId, setLastRunningNodeId] = useState<string | null>(
    null,
  );
  useEffect(() => {
    if (runningExec) {
      setLastRunningNodeId(runningExec.ir_node_id);
    }
  }, [runningExec]);
  // Clear the cache when the run reaches a terminal state so we don't
  // keep showing a stale "live" node after finish/fail/cancel. Paused
  // intentionally keeps the cached node — the user is mid-interaction.
  useEffect(() => {
    const status = snapshot?.run?.status;
    if (
      status === "finished" ||
      status === "failed" ||
      status === "failed_resumable" ||
      status === "cancelled"
    ) {
      setLastRunningNodeId(null);
    }
  }, [snapshot?.run?.status]);

  // Per-run component-local state outlives the store's reset() (which
  // only nukes the zustand store), so navigating run A → run B would
  // otherwise drag scrub position, selected node, pinned iterations,
  // the diff dialog, the bottom-tab pin, and the sticky last-running
  // node id into the new run — producing empty/truncated timelines,
  // "ghost" node selections (when B's IR doesn't contain A's nodes),
  // and an unexpectedly pinned bottom tab. Layout/dock preferences
  // persisted to localStorage are intentionally left alone.
  useEffect(() => {
    setScrubSeq(null);
    setManualSelectedNodeId(null);
    setFollowLiveNode(true);
    setIterationByNode(new Map());
    setLastRunningNodeId(null);
    setDiffFile(null);
    setDiffMode("");
    setBottomTabPinned(false);
  }, [runId]);

  // Live-follow node id with sticky fallback. When `runningExec` is
  // non-null we always use its node id (truth). When it's null but the
  // run is still active, we fall back to `lastRunningNodeId` — typically
  // the just-finished node — so the follow-live UI doesn't blank out
  // mid-transition. Scrubbing already null-ed `runningExec` above, so
  // we don't double-check `scrubSeq` here.
  const followLiveNodeId = useMemo(() => {
    if (runningExec) return runningExec.ir_node_id;
    if (scrubSeq !== null) return null;
    const status = snapshot?.run?.status;
    if (status === "running" || status === "paused_waiting_human") {
      return lastRunningNodeId;
    }
    return null;
  }, [runningExec, scrubSeq, snapshot?.run?.status, lastRunningNodeId]);

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

  // When scrubbing, derive a virtual snapshot at the chosen seq.
  // Otherwise use the live executions map.
  //
  // The range input on the Scrubber can fire onChange faster than 60 Hz
  // on drag, and `buildExecutionsAt` folds the full events array (up to
  // MAX_EVENTS = 5000) on each call. useDeferredValue lets React keep
  // the slider responsive while the heavier downstream computations
  // (executions snapshot, filtered events) catch up one frame behind.
  const deferredScrubSeq = useDeferredValue(scrubSeq);
  const displayedExecutions = useMemo(() => {
    if (deferredScrubSeq === null) return liveExecutions;
    return buildExecutionsAt(events, deferredScrubSeq);
  }, [deferredScrubSeq, events, liveExecutions]);

  const displayedEvents = useMemo(() => {
    if (deferredScrubSeq === null) return events;
    return events.filter((e) => e.seq <= deferredScrubSeq);
  }, [deferredScrubSeq, events]);

  // Fold llm_request and node_finished events into a per-node "what
  // actually ran" map. Latest event wins because seq is monotonic.
  // displayedEvents (not raw) so the time-travel scrubber rewinds too.
  // llm_request carries mid-flight overrides (claw); node_finished
  // carries the executor-stamped effective model + context window
  // (claude_code) via output._model / _context_*. See
  // pkg/backend/model/executor.go stampDelegateOutputMeta.
  const runtimeOverrideByNode = useMemo(() => {
    const m = new Map<string, RuntimeLLMOverride>();
    const update = (nodeID: string, patch: Partial<RuntimeLLMOverride>) => {
      const prev = m.get(nodeID) ?? {};
      m.set(nodeID, { ...prev, ...patch });
    };
    for (const e of displayedEvents) {
      if (!e.node_id) continue;
      const data = e.data ?? {};
      if (e.type === "llm_request") {
        const patch: Partial<RuntimeLLMOverride> = {};
        if (typeof data.model === "string") patch.model = data.model;
        if (typeof data.reasoning_effort === "string")
          patch.reasoning_effort = data.reasoning_effort;
        if (patch.model || patch.reasoning_effort) update(e.node_id, patch);
        continue;
      }
      if (e.type === "node_finished") {
        const patch = readNodeOutputMeta(
          data.output as Record<string, unknown> | undefined,
        );
        if (Object.keys(patch).length > 0) update(e.node_id, patch);
      }
    }
    return m;
  }, [displayedEvents]);

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
  // The browser pane mounts on the right column only when the user
  // chose that dock AND there's something to display. Otherwise it
  // either stays in the bottom tab list, or stays hidden.
  const browserRightDocked = browserDock === "right" && browserAvailable;

  // Pre-pickup state: the run sits on the NATS queue. RunMetrics and
  // Scrubber render nothing useful (no events yet, no budget consumed),
  // so we swap them for the QueuedBanner that surfaces position and
  // exposes a cancel button. The IR canvas stays mounted underneath so
  // the workflow shape is still visible. See cloud-ready plan §F (T-15).
  const isQueued = snapshot.run.status === "queued";

  return (
    <ReactFlowProvider>
      <div className="h-screen w-screen overflow-hidden flex flex-col bg-surface-0 text-fg-default">
        <RunHeader run={snapshot.run} active={active} wsState={wsState} />
        {isQueued ? (
          <QueuedBanner run={snapshot.run} />
        ) : (
          <>
            <RunMetrics active={active} onJumpToFailed={handleJumpToFailed} />
            <HumanInteractionPanel runId={runId} />
            <Scrubber
              events={events}
              liveSeq={liveSeq}
              scrubSeq={scrubSeq}
              onChange={setScrubSeq}
              visible={liveSeq > 0}
            />
          </>
        )}
      <div className="flex-1 min-h-0 flex">
        <LeftPanel
          runId={runId}
          run={snapshot.run}
          onSelectFile={handleSelectFile}
          onMergeComplete={refreshSnapshot}
        />
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
                // The Group's panel set changes when the browser docks
                // right, so use a different layout key (and React key)
                // for that mode — react-resizable-panels otherwise keeps
                // the previous flexGrow distribution and flips badly.
                key={browserRightDocked ? "with-browser" : "no-browser"}
                defaultLayout={
                  browserRightDocked
                    ? horizontalLayoutWithBrowser.layout
                    : horizontalLayout.layout
                }
                onLayoutChanged={
                  browserRightDocked
                    ? horizontalLayoutWithBrowser.onChange
                    : horizontalLayout.onChange
                }
              >
                <Panel
                  id="canvas"
                  defaultSize={browserRightDocked ? 50 : 70}
                  minSize={30}
                  className="min-h-0"
                >
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
                      defaultSize={browserRightDocked ? 25 : 30}
                      minSize={18}
                      className="min-h-0"
                    >
                      <div className="h-full border-l border-border-default min-h-0 overflow-hidden animate-fade-in-opacity">
                        <NodeDetailPanel
                          runId={runId}
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
                        />
                      </div>
                    </Panel>
                  </>
                )}
                {browserRightDocked && (
                  <>
                    <ResizeSeparator orientation="horizontal" />
                    <Panel
                      id="browserRight"
                      defaultSize={25}
                      minSize={20}
                      className="min-h-0"
                    >
                      <div className="h-full border-l border-border-default min-h-0 overflow-hidden animate-fade-in-opacity">
                        <BrowserPane
                          runId={runId}
                          scrubSeq={scrubSeq}
                          dock={browserDock}
                          onDockChange={setBrowserDock}
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
                      onValueChange={(v) =>
                        handleSetBottomTab(
                          v as "events" | "logs" | "report" | "browser",
                        )
                      }
                      items={[
                        { value: "events", label: "Events" },
                        { value: "logs", label: "Logs" },
                        { value: "report", label: "Report" },
                        ...(browserAvailable && !browserRightDocked
                          ? [{ value: "browser", label: "Browser" }]
                          : []),
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
                      ) : bottomTab === "logs" ? (
                        <RunLogPanel
                          runId={runId}
                          subscribeLogs={wsHandle.subscribeLogs}
                          unsubscribeLogs={wsHandle.unsubscribeLogs}
                          onCollapse={toggleEventlogCollapsed}
                        />
                      ) : bottomTab === "browser" && runId ? (
                        <BrowserPane
                          runId={runId}
                          scrubSeq={scrubSeq}
                          dock={browserDock}
                          onDockChange={setBrowserDock}
                        />
                      ) : (
                        <ReportTab onSelectNode={handleSelectNode} />
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
          mode={diffMode}
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
