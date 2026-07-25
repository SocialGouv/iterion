import { useEffect, useMemo, useState } from "react";

import type { ArtifactSummary, ExecutionState, RunEvent } from "@/api/runs";
import { listArtifacts } from "@/api/runs";
import { Tabs } from "@/components/ui";

import { ArtifactTab } from "./detail/ArtifactTab";
import { CollapseButton } from "./detail/CollapseButton";
import { DetailHeader } from "./detail/DetailHeader";
import { EventsTab } from "./detail/EventsTab";
import { FollowLivePill } from "./detail/FollowLivePill";
import { LogsTab } from "./detail/LogsTab";
import { useLLMSteps, LLMTraceView } from "./detail/LLMTrace";
import type { TabValue } from "./detail/nodeDetailFormat";
import { PauseTab } from "./detail/PauseTab";
import { useExecutionEvents } from "./detail/useExecutionEvents";
import { useToolCalls, ToolCallList } from "./detail/ToolCalls";

interface Props {
  runId: string;
  // The run's current status. Drives the no-selection empty state: a
  // terminal run must not claim to be "live" / waiting for the engine.
  runStatus?: string;
  // The workflow source path for this run; used to wire "Open in editor".
  filePath?: string;
  // All executions of the selected IR node, ordered by start time.
  // Empty array = no node selected. The active `exec` is derived from
  // this list + `selectedIteration` (a 0-based array index) inside the
  // panel so the iteration pills can switch which exec drives every
  // tab without round-trip through the parent.
  executions: ExecutionState[];
  // 0-based index into `executions` of the currently selected attempt.
  selectedIteration: number;
  onSelectIteration: (nodeId: string, index: number) => void;
  events: RunEvent[];
  // followLive == true → the parent is auto-tracking the running
  // execution; clicking the toggle off pins the panel on the current
  // exec. Clicking it on again re-engages auto-tracking, which the
  // parent implements by clearing the manual pin in handleToggle.
  followLive?: boolean;
  onToggleFollowLive?: () => void;
  // Imperative log subscription handles forwarded from RunView. The
  // per-node Logs tab subscribes while it's mounted; the hook
  // ref-counts so this coexists with the bottom RunLogPanel.
  subscribeLogs: (fromOffset?: number) => void;
  unsubscribeLogs: () => void;
  onCollapse?: () => void;
  // Byte offset to clamp the per-node Logs tab to during scrub /
  // replay. Forwarded from RunView's events[scrubSeq].log_offset so
  // the panel mirrors the bottom-log panel's rewind.
  logClampBytes?: number | null;
}

export default function NodeDetailPanel({
  runId,
  runStatus,
  filePath,
  executions,
  selectedIteration,
  onSelectIteration,
  events,
  followLive,
  onToggleFollowLive,
  subscribeLogs,
  unsubscribeLogs,
  onCollapse,
  logClampBytes = null,
}: Props) {
  const [artifactVersions, setArtifactVersions] = useState<ArtifactSummary[]>([]);
  const [activeTab, setActiveTab] = useState<TabValue | null>(null);

  // The active execution drives every tab. selectedIteration is the
  // 0-based index in `executions` (NOT a scalar loop_iteration — see
  // RunCanvasIR.defaultIterationFor comment). Clamp on out-of-range
  // so the panel stays useful when the selection points past the
  // current array length (e.g. transient race during a fan-in).
  const exec = useMemo<ExecutionState | null>(() => {
    if (executions.length === 0) return null;
    const i = Math.min(Math.max(selectedIteration, 0), executions.length - 1);
    return executions[i] ?? null;
  }, [executions, selectedIteration]);

  // Load only the version index here; ArtifactDiff handles fetching the
  // body for each selected version on demand.
  //
  // Depending on `exec` as a whole would refire on every WS event
  // (the parent rebuilds the executions array on each event, which
  // re-derives `exec` to a fresh object identity even for the same
  // logical iteration). Pin the effect to the stable scalars only —
  // a node change OR an iteration switch — and abort the in-flight
  // request when those keys change so we never thrash the network
  // pool with redundant /artifacts fetches.
  const execNodeId = exec?.ir_node_id ?? null;
  const executionId = exec?.execution_id ?? null;
  useEffect(() => {
    setArtifactVersions([]);
    if (!execNodeId) return;
    const ctrl = new AbortController();
    listArtifacts(runId, execNodeId, { signal: ctrl.signal })
      .then((summaries) => {
        if (ctrl.signal.aborted) return;
        setArtifactVersions(summaries);
      })
      .catch(() => {
        // Artifacts are best-effort — silent fall-through.
      });
    return () => {
      ctrl.abort();
    };
  }, [runId, execNodeId, executionId]);

  const matching = useExecutionEvents(events, exec);
  const llmSteps = useLLMSteps(matching);
  const toolCalls = useToolCalls(matching);

  // Tab default depends on what's most useful for the node kind.
  // We only reset on *node* change (not iteration change within the
  // same node) so flipping between iterations keeps whichever tab the
  // user was reading. Each iteration's tab content rerenders against
  // the new exec via the `matching` events feed.
  //
  // Exception: a paused exec auto-focuses the Pause tab — that one is
  // load-bearing for the human-input flow, so we promote it whenever
  // the active iteration's status is paused (even mid-node).
  const nodeId = exec?.ir_node_id ?? null;
  const isPausedExec = exec?.status === "paused_waiting_human";
  // Derive kind as a stable scalar so we can add it to the effect
  // dep list without resetting on every exec reference change. Without
  // this, an exec that arrives in the same React batch as a nodeId
  // change has the stale closure see exec === null and set
  // activeTab = null, blanking the panel.
  const execKind = exec?.kind ?? null;
  useEffect(() => {
    if (!execKind) {
      setActiveTab(null);
      return;
    }
    if (isPausedExec) {
      setActiveTab("pause");
      return;
    }
    if (execKind === "agent" || execKind === "judge") setActiveTab("trace");
    else if (execKind === "tool") setActiveTab("tools");
    else setActiveTab("events");
    // Iteration flips reuse the user's tab pick — that's why we don't
    // dep on the full `exec` object. nodeId + execKind + isPausedExec
    // cover the cases that warrant a default-tab reset.
  }, [nodeId, execKind, isPausedExec]);

  if (!exec) {
    // Terminal runs (finished / failed / cancelled) will never start
    // another node, so the "following the running node" promise — and
    // its live badge — would be a lie. Swap in a plain retrospective
    // prompt instead; running/paused runs keep the live-follow copy.
    const isTerminal =
      runStatus === "finished" ||
      runStatus === "failed" ||
      runStatus === "cancelled";
    return (
      <div className="relative h-full p-4 text-xs text-fg-subtle">
        <CollapseButton onCollapse={onCollapse} />
        {!isTerminal && onToggleFollowLive && (
          <FollowLivePill
            followLive={!!followLive}
            onToggle={onToggleFollowLive}
          />
        )}
        {isTerminal ? (
          <p className="mt-8">
            Run {runStatus === "cancelled" ? "was cancelled" : runStatus} —
            select a node to inspect its output, events, and trace.
          </p>
        ) : followLive ? (
          <p className="mt-8">
            Following the running node. Nothing is executing right now —
            this panel will jump in as soon as the engine starts a node.
          </p>
        ) : (
          <p className="mt-8">
            Click a node to see its events, prompt, response, artifact,
            and error trace.
          </p>
        )}
      </div>
    );
  }

  const hasArtifact = artifactVersions.length > 0;
  const isPaused = exec.status === "paused_waiting_human";

  // Tab order: Pause first when paused (drives action), then Trace,
  // Tools, Artifact, Events. Pause hidden when not paused.
  const tabItems: Array<{ value: TabValue; label: string; disabled?: boolean }> = [
    ...(isPaused
      ? [{ value: "pause" as TabValue, label: "Pause" }]
      : []),
    {
      value: "trace" as TabValue,
      label: llmSteps.length > 1 ? `Trace (${llmSteps.length})` : "Trace",
      disabled: llmSteps.length === 0,
    },
    {
      value: "tools" as TabValue,
      label: `Tools (${toolCalls.length})`,
      disabled: toolCalls.length === 0,
    },
    {
      value: "artifact" as TabValue,
      label:
        artifactVersions.length > 1
          ? `Artifact (${artifactVersions.length})`
          : "Artifact",
      disabled: !hasArtifact,
    },
    { value: "events" as TabValue, label: `Events (${matching.length})` },
    { value: "logs" as TabValue, label: "Logs" },
  ];

  return (
    <div className="relative h-full flex flex-col text-xs">
      <CollapseButton onCollapse={onCollapse} />
      <DetailHeader
        runId={runId}
        filePath={filePath}
        exec={exec}
        executions={executions}
        selectedIteration={selectedIteration}
        onSelectIteration={onSelectIteration}
        events={matching}
        followLive={followLive}
        onToggleFollowLive={onToggleFollowLive}
      />

      <Tabs
        value={activeTab ?? "events"}
        onValueChange={(v) => setActiveTab(v as TabValue)}
        items={tabItems}
        variant="underline"
        listClassName="px-3"
        className="flex-1 min-h-0"
        panels={{
          pause: (
            <PauseTab
              runId={runId}
              nodeId={exec.ir_node_id}
              matching={matching}
            />
          ),
          trace: (
            <div className="overflow-auto px-4 py-3 h-full">
              {llmSteps.length === 0 ? (
                <p className="text-fg-subtle">
                  No LLM activity recorded for this execution.
                </p>
              ) : (
                <LLMTraceView steps={llmSteps} />
              )}
            </div>
          ),
          tools: (
            <div className="overflow-auto px-4 py-3 h-full">
              {toolCalls.length === 0 ? (
                <div className="text-fg-subtle">
                  No tool calls for this execution.
                </div>
              ) : (
                <ToolCallList calls={toolCalls} runId={runId} />
              )}
            </div>
          ),
          artifact: (
            <ArtifactTab
              runId={runId}
              nodeId={exec.ir_node_id}
              versions={artifactVersions}
            />
          ),
          events: (
            <div className="overflow-hidden h-full">
              <EventsTab events={matching} />
            </div>
          ),
          logs: (
            <LogsTab
              runId={runId}
              subscribeLogs={subscribeLogs}
              unsubscribeLogs={unsubscribeLogs}
              filterNodeId={exec.ir_node_id}
              filterIteration={exec.loop_iteration}
              clampToBytes={logClampBytes}
            />
          ),
        }}
      />
    </div>
  );
}

// Re-export needed by EventLog (Phase 6 will own it).
export type { TabValue };
