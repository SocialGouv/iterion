import { useCallback, useEffect, useState } from "react";

// useSelectionState owns the run console's workflow-view selection
// dials, lifted verbatim out of RunView:
//   - manualSelectedNodeId: the user's most recent canvas click. Pinned
//     until they click another node or toggle follow-live back on.
//   - followLiveNode: when true, the panel auto-shifts to whatever node
//     is currently running; when false, manualSelectedNodeId wins.
//   - iterationByNode: per-IR-node iteration override. Empty map means
//     "use the default (running > paused > latest)". A user click on a
//     timeline pip sets the entry; we never auto-clear so the user's
//     pick stays sticky.
//   - scrubSeq: time-travel scrubber position. When non-null, the
//     canvas/detail/event log render the run *as it was* at this seq.
//     When null (the default), live data flows through.
//
// Per-run state outlives the store's reset() (which only nukes the
// zustand store), so navigating run A → run B would otherwise drag
// scrub position, selected node, pinned iterations, and any "ghost"
// node selections (when B's IR doesn't contain A's nodes) into the
// new run. The runId/attempt-keyed reset effect snaps everything back to
// defaults; layout/dock preferences persisted to localStorage are
// intentionally left alone (they live in useRunConsoleLayout).
export interface SelectionState {
  scrubSeq: number | null;
  setScrubSeq: (next: number | null) => void;
  manualSelectedNodeId: string | null;
  followLiveNode: boolean;
  iterationByNode: Map<string, number>;
  handleSelectIteration: (nodeId: string, iteration: number) => void;
  handleSelectNode: (nodeId: string | null) => void;
  handleJumpToFailed: (nodeId: string) => void;
  handleEventSelect: (nodeId: string, iteration: number) => void;
  handleClearSelection: () => void;
  handleToggleFollowLive: () => void;
}

export function useSelectionState(runId: string | null, attemptKey?: string): SelectionState {
  const [scrubSeq, setScrubSeq] = useState<number | null>(null);
  // Workflow view selects by IR node id (not execution id). The detail
  // panel is driven by the selected node's currently-picked iteration
  // (per-node, default = "current"). Manual picks live alongside a
  // "follow live" toggle: when the toggle is on, the panel auto-shifts
  // to whatever node is currently running; when the user clicks a node
  // the toggle flips off so their pick stays pinned.
  const [manualSelectedNodeId, setManualSelectedNodeId] = useState<string | null>(
    null,
  );
  const [followLiveNode, setFollowLiveNode] = useState<boolean>(true);
  // Per-IR-node iteration override. Empty map means "use the default
  // (running > paused > latest)". A user click on a timeline pip sets
  // the entry; we never auto-clear so the user's pick stays sticky.
  const [iterationByNode, setIterationByNode] = useState<Map<string, number>>(
    () => new Map(),
  );

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

  const handleSelectNode = useCallback((nodeId: string | null) => {
    setManualSelectedNodeId(nodeId);
    // Click on a real node pins it (and disables follow-live).
    // Click on empty pane / toggle-off does the opposite: re-engage
    // auto-follow so the user has an obvious "go back to live" path
    // without hunting for the FollowLivePill inside the detail panel.
    setFollowLiveNode(nodeId === null);
  }, []);

  const handleJumpToFailed = useCallback(
    (nodeId: string) => {
      handleSelectNode(nodeId);
    },
    [handleSelectNode],
  );

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

  // Per-run reset — see file-level comment for the rationale. Lives in
  // this hook because every piece of state it nukes is owned here.
  useEffect(() => {
    setScrubSeq(null);
    setManualSelectedNodeId(null);
    setFollowLiveNode(true);
    setIterationByNode(new Map());
  }, [runId, attemptKey]);

  return {
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
  };
}
