import { useEffect, useState } from "react";
import { useLocation } from "wouter";

import type { ExecutionState, RunEvent } from "@/api/runs";
import { CopyButton, IconButton, StatusBadge } from "@/components/ui";
import {
  formatContextUsage,
  formatDurationBetween,
  formatMs,
  formatTime,
} from "@/lib/format";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";
import { useNodeLabel } from "@/lib/runChat/useNodeLabel";

import { FollowLivePill } from "./FollowLivePill";
import { IterationCrumb } from "./IterationCrumb";
import { IterationPills } from "./IterationPills";
import { NodeKindIcon } from "./NodeKindIcon";
import { useExecutionCostMeta } from "./useExecutionCostMeta";

export function DetailHeader({
  runId,
  filePath,
  exec,
  executions,
  selectedIteration,
  onSelectIteration,
  events,
  followLive,
  onToggleFollowLive,
}: {
  runId: string;
  filePath?: string;
  exec: ExecutionState;
  executions: ExecutionState[];
  // 0-based index into `executions` of the currently selected attempt.
  selectedIteration: number;
  onSelectIteration: (nodeId: string, index: number) => void;
  events: RunEvent[];
  followLive?: boolean;
  onToggleFollowLive?: () => void;
}) {
  const [, setLocation] = useLocation();
  const nodeLabel = useNodeLabel();
  const duration = formatDurationBetween(exec.started_at, exec.finished_at);
  const {
    costUsd,
    tokens,
    model,
    contextWindow,
    contextUsed,
    thinkingTokens,
    thinkingMs,
  } = useExecutionCostMeta(events);
  const contextUsage = formatContextUsage(contextUsed, contextWindow);
  // Wall-clock vs relative durations. Persisted so the preference
  // survives reloads — operators correlating runs with CI/dashboards
  // need wall-clock anchors and shouldn't have to re-toggle each visit.
  const [showAbsoluteTimes, setShowAbsoluteTimes] = useState<boolean>(() =>
    readBooleanFlag("run-console.absolute-times"),
  );
  useEffect(() => {
    writeBooleanFlag("run-console.absolute-times", showAbsoluteTimes);
  }, [showAbsoluteTimes]);

  return (
    <div className="px-4 pt-3 pb-3 pr-10 border-b border-border-default">
      <div className="flex items-start gap-2">
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 mb-1">
            <StatusBadge status={exec.status} />
            <NodeKindIcon kind={exec.kind} />
            <h2 className="text-sm font-semibold truncate" title={exec.ir_node_id}>
              {nodeLabel(exec.ir_node_id)}
              <span className="ml-2 font-mono font-normal text-micro text-fg-subtle">
                {exec.ir_node_id}
              </span>
            </h2>
            {onToggleFollowLive && (
              <FollowLivePill
                followLive={!!followLive}
                onToggle={onToggleFollowLive}
              />
            )}
          </div>
          <div className="text-fg-subtle text-caption flex flex-wrap gap-x-3 gap-y-0.5">
            {exec.kind && <span>kind: {exec.kind}</span>}
            <span>branch: {exec.branch_id}</span>
            <IterationCrumb
              exec={exec}
              executions={executions}
              selectedIteration={selectedIteration}
              onSelect={(iter) => onSelectIteration(exec.ir_node_id, iter)}
            />
            {duration && (
              <button
                type="button"
                onClick={() => setShowAbsoluteTimes((v) => !v)}
                aria-pressed={showAbsoluteTimes}
                className="hover:text-fg-default text-left"
                title={
                  showAbsoluteTimes
                    ? "Click for relative duration"
                    : "Click for absolute wall-clock anchors"
                }
              >
                {showAbsoluteTimes && exec.started_at
                  ? `${formatTime(exec.started_at)} → ${
                      exec.finished_at ? formatTime(exec.finished_at) : "…"
                    }`
                  : `duration: ${duration}`}
              </button>
            )}
            {tokens > 0 && <span>tokens: {tokens.toLocaleString()}</span>}
            {(thinkingTokens > 0 || thinkingMs > 0) && (
              <span
                title="Extended thinking. Token count is an approximation (the provider bills thinking inside output tokens; the text is re-encoded). Time is measured (exact for claw, best-effort for claude_code)."
              >
                🧠 ~{thinkingTokens.toLocaleString()} tok · {formatMs(thinkingMs)}
              </span>
            )}
            {costUsd > 0 && (
              <span title={`$${costUsd.toFixed(6)}`}>
                cost: ${costUsd.toFixed(4)}
              </span>
            )}
            {model && <span className="font-mono">{model}</span>}
            {contextUsage && (
              <span title={contextUsage.title}>
                ctx: {contextUsage.label} ({Math.round(contextUsage.pct)}%)
              </span>
            )}
          </div>
          {executions.length > 1 && (
            <IterationPills
              executions={executions}
              selectedIteration={selectedIteration}
              onSelect={(iter) => onSelectIteration(exec.ir_node_id, iter)}
            />
          )}
        </div>
        {filePath && (
          <IconButton
            label="Open in editor"
            tooltip="Open this node in the studio"
            size="sm"
            variant="ghost"
            onClick={() =>
              setLocation(
                `/editor?file=${encodeURIComponent(filePath)}&node=${encodeURIComponent(
                  exec.ir_node_id,
                )}&from=${encodeURIComponent(runId)}`,
              )
            }
          >
            ↗
          </IconButton>
        )}
      </div>

      {exec.error && (
        <div className="mt-2 px-2 py-1.5 rounded bg-danger-soft text-danger-fg">
          <div className="flex items-center justify-between gap-2 mb-0.5">
            <span className="font-medium">Error</span>
            <CopyButton value={exec.error} />
          </div>
          <div className="font-mono text-micro whitespace-pre-wrap break-words">
            {exec.error}
          </div>
        </div>
      )}
    </div>
  );
}
