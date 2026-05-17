import {
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useLocation } from "wouter";
import { ChevronRightIcon } from "@radix-ui/react-icons";

import type { ArtifactSummary, ExecutionState, RunEvent } from "@/api/runs";
import { fetchToolBlob, listArtifacts } from "@/api/runs";
import { formatBytes } from "@/lib/format";
import { IconButton, Input, StatusBadge, Tabs } from "@/components/ui";
import { stepIteration } from "@/lib/eventIter";
import { formatContextUsage, formatDurationBetween, formatMs } from "@/lib/format";
import { readNodeOutputMeta } from "@/lib/delegateMeta";

import ArtifactDiff from "./ArtifactDiff";
import { iterationColor } from "./IRNode";
import LogLinesView from "./LogLinesView";
import PauseForm from "./PauseForm";
import { statusClasses } from "./runStatusClasses";
import { formatToolCall, type ToolField, type TodoItem } from "./toolFormatters";

interface Props {
  runId: string;
  // The .iter source path for this run; used to wire "Open in editor".
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

function CollapseButton({ onCollapse }: { onCollapse?: () => void }): ReactNode {
  if (!onCollapse) return null;
  return (
    <IconButton
      label="Hide details panel"
      size="sm"
      variant="ghost"
      className="absolute top-1.5 right-1.5 z-10"
      onClick={onCollapse}
    >
      <ChevronRightIcon />
    </IconButton>
  );
}

// FollowLivePill toggles the parent's auto-tracking of the running
// node. When active, the panel jumps to whatever the engine is
// currently working on; when off, the user's manual selection stays
// pinned. The visual is a pill with a pulsing dot when active so it
// reads as "live" at a glance.
function FollowLivePill({
  followLive,
  onToggle,
}: {
  followLive: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onToggle}
      className={`inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] border transition-colors ${
        followLive
          ? "bg-success-soft border-success text-success-fg"
          : "bg-surface-1 border-border-default text-fg-subtle hover:text-fg-default"
      }`}
      title={
        followLive
          ? "Auto-following the running node. Click to pin on the current selection."
          : "Pinned. Click to follow the running node."
      }
    >
      <span
        className={`inline-block w-1.5 h-1.5 rounded-full ${
          followLive ? "bg-success animate-pulse" : "bg-fg-subtle"
        }`}
      />
      live
    </button>
  );
}

// IterationPills mirrors the per-iteration pip strip already shown on
// the canvas node (IRNode), but in the right-panel header. The colors
// come from the same ITERATION_PALETTE so a user scanning the canvas
// for "iter 3" sees the same amber tint in both places. Status is
// overlaid as a ring/animation so the pill carries two dimensions:
// which iteration (color) and how it went (ring/pulse/opacity).
function IterationPills({
  executions,
  selectedIteration,
  onSelect,
}: {
  executions: ExecutionState[];
  // 0-based index into `executions` of the currently selected attempt.
  selectedIteration: number;
  // Callback receives the selected attempt's array index.
  onSelect: (index: number) => void;
}) {
  return (
    <div className="mt-1 flex flex-wrap items-center gap-1">
      <span className="text-[9px] text-fg-subtle mr-0.5">iter:</span>
      {executions.map((e, idx) => {
        const isSelected = idx === selectedIteration;
        const s = statusClasses(e.status);
        const color = iterationColor(idx);
        // Selection is rendered as a thicker ring in accent color so
        // the active pill pops; the iteration color stays as the
        // fill. Status drives extra cues:
        //   running → animate-pulse (matches StatusBadge running)
        //   failed  → red ring overlay
        //   skipped → desaturated/opacity (engine bypassed this iter)
        const pulse = e.status === "running" ? "animate-pulse" : "";
        const opacity = e.status === "skipped" ? "opacity-50" : "";
        const failedRing = e.status === "failed" ? "ring-1 ring-danger" : "";
        const selectedRing = isSelected
          ? "ring-2 ring-accent shadow-sm"
          : "ring-1 ring-border-default/30";
        return (
          <button
            key={e.execution_id}
            type="button"
            onClick={() => onSelect(idx)}
            title={`iter ${idx + 1} · ${s.label}`}
            className={`inline-flex items-center justify-center min-w-[18px] h-[18px] px-1 rounded-full text-[9px] font-mono text-fg-default transition-all ${pulse} ${opacity} ${selectedRing} ${failedRing}`}
            style={{ backgroundColor: `${color}66` }}
          >
            {idx + 1}
          </button>
        );
      })}
    </div>
  );
}

type TabValue = "pause" | "trace" | "tools" | "artifact" | "events" | "logs";

export default function NodeDetailPanel({
  runId,
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
  const pause = usePauseInfo(matching);

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
    return (
      <div className="relative h-full p-4 text-xs text-fg-subtle">
        <CollapseButton onCollapse={onCollapse} />
        {onToggleFollowLive && (
          <FollowLivePill
            followLive={!!followLive}
            onToggle={onToggleFollowLive}
          />
        )}
        {followLive ? (
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
            <div className="overflow-auto px-4 py-3 h-full">
              <PauseForm
                runId={runId}
                questions={pause?.questions ?? {}}
                message={pause?.message}
              />
            </div>
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
            <div className="overflow-auto px-4 py-3 h-full">
              {!hasArtifact ? (
                <div className="text-fg-subtle">No artifact published.</div>
              ) : (
                <ArtifactDiff
                  runId={runId}
                  nodeId={exec.ir_node_id}
                  versions={artifactVersions}
                />
              )}
            </div>
          ),
          events: (
            <div className="overflow-hidden h-full">
              <EventsTabContent events={matching} />
            </div>
          ),
          logs: (
            <LogLinesView
              runId={runId}
              subscribeLogs={subscribeLogs}
              unsubscribeLogs={unsubscribeLogs}
              filterNodeId={exec.ir_node_id}
              filterIteration={exec.loop_iteration}
              showTitle={false}
              clampToBytes={logClampBytes}
            />
          ),
        }}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

function DetailHeader({
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
  const duration = formatDurationBetween(exec.started_at, exec.finished_at);
  // Per-execution cost + tokens are sourced from the node_finished
  // event the runtime emits with the cost.Annotate output. A single
  // execution emits at most one node_finished, so summing across the
  // matching events is just to defensively merge if the engine ever
  // emits multiple (e.g. on retry within a node) — the common case is
  // exactly one row.
  const { costUsd, tokens, model, contextWindow, contextUsed } = useMemo(() => {
    let costUsd = 0;
    let tokens = 0;
    let model = "";
    let contextWindow = 0;
    let contextUsed = 0;
    for (const e of events) {
      if (e.type !== "node_finished" || !e.data) continue;
      const c = e.data["_cost_usd"];
      if (typeof c === "number") costUsd += c;
      const t = e.data["_tokens"];
      if (typeof t === "number") tokens += t;
      const meta = readNodeOutputMeta(
        e.data["output"] as Record<string, unknown> | undefined,
      );
      if (meta.model && !model) model = meta.model;
      if (meta.contextWindow && meta.contextWindow > contextWindow)
        contextWindow = meta.contextWindow;
      if (meta.contextUsed && meta.contextUsed > contextUsed)
        contextUsed = meta.contextUsed;
    }
    return { costUsd, tokens, model, contextWindow, contextUsed };
  }, [events]);
  const contextUsage = formatContextUsage(contextUsed, contextWindow);
  const [copied, setCopied] = useState(false);
  const onCopyError = async () => {
    if (!exec.error) return;
    try {
      await navigator.clipboard.writeText(exec.error);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      // ignore — clipboard may be unavailable
    }
  };

  return (
    <div className="px-4 pt-3 pb-3 pr-10 border-b border-border-default">
      <div className="flex items-start gap-2">
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 mb-1">
            <StatusBadge status={exec.status} />
            <h2 className="text-sm font-semibold truncate" title={exec.ir_node_id}>
              {exec.ir_node_id}
            </h2>
            {onToggleFollowLive && (
              <FollowLivePill
                followLive={!!followLive}
                onToggle={onToggleFollowLive}
              />
            )}
          </div>
          <div className="text-fg-subtle text-[10px] flex flex-wrap gap-x-3 gap-y-0.5">
            {exec.kind && <span>kind: {exec.kind}</span>}
            <span>branch: {exec.branch_id}</span>
            <span>
              iter:{" "}
              {(() => {
                // Display 1-based position in the start-ordered
                // executions array. Scalar `loop_iteration` is no
                // longer unique post-Option-3 (the runtime's
                // currentLoopIteration returns max() across containing
                // loops so an outer-loop counter can dominate every
                // attempt of an inner loop), so it cannot be shown
                // raw here.
                const idx = executions.findIndex(
                  (e) => e.execution_id === exec.execution_id,
                );
                return idx >= 0 ? idx + 1 : 1;
              })()}
            </span>
            {duration && <span>duration: {duration}</span>}
            {tokens > 0 && <span>tokens: {tokens.toLocaleString()}</span>}
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
            tooltip="Open this node in the editor"
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
            <button
              type="button"
              onClick={() => void onCopyError()}
              className="text-[10px] text-danger-fg/80 hover:text-danger-fg underline"
            >
              {copied ? "copied" : "copy"}
            </button>
          </div>
          <div className="font-mono text-[11px] whitespace-pre-wrap break-words">
            {exec.error}
          </div>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Trace tab
// ---------------------------------------------------------------------------

interface LLMStep {
  seq: number;
  systemPrompt?: string;
  userMessage?: string;
  response?: string;
  model?: string;
  inputTokens?: number;
  outputTokens?: number;
  finishReason?: string;
  toolCalls?: Array<{ name: string; input?: string }>;
  // True for the in-flight step (request emitted, no finished event
  // yet) — rendered as a "pending" card.
  pending?: boolean;
}

function useLLMSteps(matching: RunEvent[]): LLMStep[] {
  return useMemo<LLMStep[]>(() => {
    const steps: LLMStep[] = [];
    let current: LLMStep | null = null;
    let lastModel: string | undefined;
    for (const e of matching) {
      if (e.type === "llm_prompt" && e.data) {
        // A new step begins. If a previous step was still pending,
        // close it as-is; the new prompt overrides what came before.
        if (current) steps.push(current);
        current = {
          seq: e.seq,
          systemPrompt: (e.data["system_prompt"] as string) ?? undefined,
          userMessage: (e.data["user_message"] as string) ?? undefined,
          model: lastModel,
          pending: true,
        };
      } else if (e.type === "llm_request" && e.data) {
        const model = (e.data["model"] as string) ?? undefined;
        if (model) lastModel = model;
        if (current) current.model = model ?? current.model;
        else
          current = {
            seq: e.seq,
            model: model ?? lastModel,
            pending: true,
          };
      } else if (e.type === "llm_step_finished" && e.data) {
        if (!current) current = { seq: e.seq, pending: false };
        current.response = (e.data["response_text"] as string) ?? current.response;
        current.inputTokens =
          (e.data["input_tokens"] as number) ?? current.inputTokens;
        current.outputTokens =
          (e.data["output_tokens"] as number) ?? current.outputTokens;
        current.finishReason =
          (e.data["finish_reason"] as string) ?? current.finishReason;
        const calls = e.data["tool_call_details"] as
          | Array<Record<string, unknown>>
          | undefined;
        if (calls) {
          current.toolCalls = calls.map((c) => ({
            name: (c["tool_name"] as string) ?? "",
            input: c["input"] as string | undefined,
          }));
        }
        current.pending = false;
        steps.push(current);
        current = null;
      }
    }
    if (current) steps.push(current);
    return steps;
  }, [matching]);
}

function LLMTraceView({ steps }: { steps: LLMStep[] }) {
  const totalIn = steps.reduce((s, x) => s + (x.inputTokens ?? 0), 0);
  const totalOut = steps.reduce((s, x) => s + (x.outputTokens ?? 0), 0);
  return (
    <div className="space-y-2">
      <div className="text-[10px] text-fg-subtle flex flex-wrap gap-x-3 gap-y-0.5">
        <span>{steps.length} step{steps.length === 1 ? "" : "s"}</span>
        {(totalIn > 0 || totalOut > 0) && (
          <span>
            tokens: in {totalIn} · out {totalOut}
          </span>
        )}
      </div>
      {steps.map((step, i) => (
        <LLMStepCard
          key={step.seq}
          index={i}
          step={step}
          // Only the most recent step is open by default — older steps
          // collapsed to keep the panel tight when a multi-step agent
          // ran for many turns.
          defaultOpen={i === steps.length - 1}
        />
      ))}
    </div>
  );
}

function LLMStepCard({
  index,
  step,
  defaultOpen,
}: {
  index: number;
  step: LLMStep;
  defaultOpen: boolean;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const summary = step.pending ? "in flight…" : step.finishReason ?? "ok";
  return (
    <div className="rounded border border-border-default bg-surface-1">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="w-full flex items-center gap-2 px-2 py-1.5 text-left hover:bg-surface-2 rounded-t"
      >
        <span className="text-[10px] font-mono text-fg-subtle">
          step {index + 1}
        </span>
        {step.pending ? (
          <span className="text-[10px] text-info-fg animate-pulse">
            ⏳ in flight
          </span>
        ) : (
          <span className="text-[10px] text-fg-muted">{summary}</span>
        )}
        {step.model && (
          <span className="text-[10px] font-mono text-fg-subtle truncate">
            {step.model}
          </span>
        )}
        {(step.inputTokens !== undefined || step.outputTokens !== undefined) && (
          <span className="text-[10px] text-fg-subtle">
            {step.inputTokens ?? 0}↑/{step.outputTokens ?? 0}↓
          </span>
        )}
        <span className="ml-auto text-fg-subtle text-[10px]">{open ? "▾" : "▸"}</span>
      </button>
      {open && (
        <div className="border-t border-border-default p-2 space-y-1">
          {step.systemPrompt && (
            <TraceBlock title="system prompt" body={step.systemPrompt} />
          )}
          {step.userMessage && (
            <TraceBlock title="user message" body={step.userMessage} />
          )}
          {step.response && (
            <TraceBlock title="response" body={step.response} defaultOpen />
          )}
          {step.toolCalls && step.toolCalls.length > 0 && (
            <details className="rounded border border-border-default" open>
              <summary className="px-2 py-1 cursor-pointer text-fg-muted bg-surface-2 rounded-t">
                tool calls ({step.toolCalls.length})
              </summary>
              <ul className="p-2 text-[10px] font-mono space-y-1">
                {step.toolCalls.map((c, i) => (
                  <li key={i}>
                    <span className="text-fg-default">{c.name}</span>
                    {c.input && (
                      <pre className="text-fg-subtle whitespace-pre-wrap">
                        {c.input}
                      </pre>
                    )}
                  </li>
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
    </div>
  );
}

function TraceBlock({
  title,
  body,
  defaultOpen = true,
}: {
  title: string;
  body: string;
  defaultOpen?: boolean;
}) {
  return (
    <details className="rounded border border-border-default" open={defaultOpen}>
      <summary className="px-2 py-1 cursor-pointer text-fg-muted bg-surface-2 rounded-t flex items-center justify-between">
        <span>{title}</span>
        <CopyButton value={body} />
      </summary>
      <pre className="p-2 text-[10px] font-mono whitespace-pre-wrap max-h-60 overflow-auto">
        {body}
      </pre>
    </details>
  );
}

// ---------------------------------------------------------------------------
// Tools tab
// ---------------------------------------------------------------------------

interface ToolCall {
  seq: number;
  toolName: string;
  isError: boolean;
  duration?: number;
  rawData: Record<string, unknown> | undefined;
  // input/output: present when the call fit inline OR carries a head
  // preview from the sidecar blob. Always renderable as the initial
  // payload view.
  input?: string;
  output?: string;
  // {input,output}Ref: when set, the full body lives in a sidecar blob
  // addressable via fetchToolBlob(runId, ref, kind). The editor renders
  // a "Load more" affordance below the preview that paginates through
  // the rest of the bytes (infinite scroll).
  inputRef?: string;
  outputRef?: string;
  // {input,output}Size: total byte size of the original body. Used to
  // size progress affordances and decide whether to offer Load more.
  inputSize?: number;
  outputSize?: number;
  errorMsg?: string;
}

// pickInlinePayload returns whatever string form of the tool call's
// `kind` payload is available on the merged event data: the full inline
// body if the call fit under the threshold, otherwise the 4 KB head
// preview the backend included alongside the sidecar blob ref. Returns
// undefined when neither is set (e.g. an empty input/output).
function pickInlinePayload(
  data: Record<string, unknown>,
  kind: "input" | "output",
): string | undefined {
  const inline = data[kind];
  if (typeof inline === "string") return inline;
  if (inline !== undefined) return safeJSON(inline);
  const preview = data[`${kind}_preview`];
  if (typeof preview === "string") return preview;
  return undefined;
}

// pickRef returns the sidecar blob ref (= tool_use_id) when present.
// The presence of a ref tells the editor "there's more to fetch beyond
// the preview" — render the Load more affordance.
function pickRef(
  data: Record<string, unknown>,
  kind: "input" | "output",
): string | undefined {
  const ref = data[`${kind}_ref`];
  return typeof ref === "string" && ref !== "" ? ref : undefined;
}

// pickSize returns the total byte size of the original tool payload as
// declared on the event (`{kind}_size`). Used to size Load more progress
// affordances. Returns undefined when the event predates this plumbing.
function pickSize(
  data: Record<string, unknown>,
  kind: "input" | "output",
): number | undefined {
  const size = data[`${kind}_size`];
  return typeof size === "number" ? size : undefined;
}

// useToolCalls merges `tool_started` (carrying the JSON input) with
// `tool_called` / `tool_error` (carrying duration, error, and the tool's
// output result) by `tool_use_id`, so the per-node Tools tab can render
// in+out side-by-side for every tool call.
//
// Falls back to no-merge when `tool_use_id` is absent on either side (legacy
// events from runs that predate this plumbing). The
// `tool_called`/`tool_error` event remains the entry's identity (its seq is
// used as the key), so already-rendered cards don't disappear from older
// runs and the in-flight state is not surfaced here (a separate concern).
function useToolCalls(matching: RunEvent[]): ToolCall[] {
  return useMemo<ToolCall[]>(() => {
    const startedByID = new Map<string, RunEvent>();
    for (const e of matching) {
      if (e.type !== "tool_started") continue;
      const id = e.data?.["tool_use_id"];
      if (typeof id === "string" && id !== "") {
        startedByID.set(id, e);
      }
    }
    return matching
      .filter((e) => e.type === "tool_called" || e.type === "tool_error")
      .map((e) => {
        const data = e.data ?? {};
        const toolUseID = data["tool_use_id"];
        const startedData =
          typeof toolUseID === "string" && toolUseID !== ""
            ? startedByID.get(toolUseID)?.data ?? {}
            : {};
        // Merge: post-execution event wins for duration/error; pre-execution
        // event provides the input payload for structured rendering. Use
        // Object.assign to keep the merged object as a fresh map.
        const merged: Record<string, unknown> = { ...startedData, ...data };
        const toolName =
          (merged["tool_name"] as string) ?? (merged["tool"] as string) ?? "unknown";
        const isError = e.type === "tool_error";
        return {
          seq: e.seq,
          toolName,
          isError,
          duration:
            typeof merged["duration_ms"] === "number"
              ? (merged["duration_ms"] as number)
              : undefined,
          rawData: merged,
          input: pickInlinePayload(merged, "input"),
          output: pickInlinePayload(merged, "output"),
          inputRef: pickRef(merged, "input"),
          outputRef: pickRef(merged, "output"),
          inputSize: pickSize(merged, "input"),
          outputSize: pickSize(merged, "output"),
          errorMsg: isError
            ? (merged["error"] as string) ?? (merged["message"] as string)
            : undefined,
        };
      });
  }, [matching]);
}

function ToolCallList({ calls, runId }: { calls: ToolCall[]; runId: string }) {
  return (
    <ul className="space-y-2">
      {calls.map((c) => (
        <ToolCallCard key={c.seq} call={c} runId={runId} />
      ))}
    </ul>
  );
}

// FETCH_CHUNK_BYTES is the page size for lazy tool-blob reads. 64 KB is
// large enough that "load more" feels responsive (one chunk fully fills
// the visible expanded pane), small enough that a megabyte-scale output
// streams in ~16 round trips rather than one giant request that blocks
// the user's first paint.
const FETCH_CHUNK_BYTES = 64 * 1024;

// ToolPayloadBlock renders the preview (or full inline body) of a tool's
// I/O payload as a collapsible <details>. When the call exceeded the
// backend's inline threshold, props `toolUseID` + `totalSize` are set:
// expanding the details shows the 4 KB preview plus a "load more"
// button that paginates through the sidecar blob via fetchToolBlob.
// The fetched bytes are appended in-component (not in the run store)
// so reopening the same card later re-fetches — keeps the in-memory
// event cache lean.
function ToolPayloadBlock({
  label,
  value,
  runId,
  toolUseID,
  kind,
  totalSize,
}: {
  label: string;
  value: string;
  runId: string;
  toolUseID?: string;
  kind: "input" | "output";
  totalSize?: number;
}) {
  const [open, setOpen] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [overflow, setOverflow] = useState(false);
  // Fetched extras concatenated after the preview. Empty until the user
  // hits "load more" / triggers an infinite-scroll boundary. Re-set on
  // collapse so the next expansion starts fresh — avoids unbounded
  // retention of MBs of stdout in the editor heap.
  const [extra, setExtra] = useState("");
  const [loading, setLoading] = useState(false);
  const [fetchErr, setFetchErr] = useState<string | null>(null);
  const preRef = useRef<HTMLPreElement>(null);

  const hasMore =
    toolUseID !== undefined &&
    totalSize !== undefined &&
    value.length + extra.length < totalSize;
  const loaded = value.length + extra.length;
  const fullValue = extra ? value + extra : value;

  useLayoutEffect(() => {
    if (!open || expanded) {
      setOverflow(false);
      return;
    }
    const el = preRef.current;
    if (!el) return;
    setOverflow(el.scrollHeight > el.clientHeight + 1);
  }, [open, expanded, fullValue]);

  const loadMore = async () => {
    if (loading || !hasMore || !toolUseID) return;
    setLoading(true);
    setFetchErr(null);
    try {
      const chunk = await fetchToolBlob(
        runId,
        toolUseID,
        kind,
        loaded,
        FETCH_CHUNK_BYTES,
      );
      setExtra((prev) => prev + chunk.data);
    } catch (err) {
      setFetchErr(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  // Infinite-scroll: when expanded and the user scrolls within ~200 px of
  // the bottom of the <pre>, kick off the next chunk fetch automatically.
  // Falls through cleanly when there's no more to fetch.
  const handleScroll = () => {
    const el = preRef.current;
    if (!el || !expanded || !hasMore || loading) return;
    const remaining = el.scrollHeight - el.scrollTop - el.clientHeight;
    if (remaining < 200) {
      void loadMore();
    }
  };

  return (
    <details
      className="mb-1"
      onToggle={(e) => {
        const next = (e.currentTarget as HTMLDetailsElement).open;
        setOpen(next);
        if (!next) {
          setExpanded(false);
          setExtra("");
          setFetchErr(null);
        }
      }}
    >
      <summary className="cursor-pointer text-[10px] text-fg-muted px-1 py-0.5 rounded hover:bg-surface-2 flex items-center justify-between">
        <span>
          {label}
          {totalSize !== undefined && totalSize > value.length && (
            <span className="ml-1 text-fg-subtle">
              ({formatBytes(loaded)} / {formatBytes(totalSize)})
            </span>
          )}
        </span>
        <CopyButton value={fullValue} />
      </summary>
      <pre
        ref={preRef}
        onScroll={handleScroll}
        className={`mt-1 text-[10px] font-mono whitespace-pre-wrap break-words bg-surface-1 rounded p-1.5 ${
          expanded ? "max-h-[60vh] overflow-auto" : "max-h-40 overflow-auto"
        }`}
      >
        {fullValue}
      </pre>
      {fetchErr && (
        <div className="mt-1 text-[10px] text-danger-fg px-1">
          fetch error: {fetchErr}
        </div>
      )}
      <div className="mt-1 flex gap-1.5 items-center">
        {(overflow || expanded) && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="text-[10px] text-fg-subtle hover:text-fg-default px-1 py-0.5 rounded hover:bg-surface-2"
          >
            {expanded ? "collapse" : "expand"}
          </button>
        )}
        {hasMore && (
          <button
            type="button"
            onClick={() => void loadMore()}
            disabled={loading}
            className="text-[10px] text-fg-subtle hover:text-fg-default px-1 py-0.5 rounded hover:bg-surface-2 disabled:opacity-50"
          >
            {loading
              ? "loading…"
              : `load more (+${formatBytes(Math.min(FETCH_CHUNK_BYTES, (totalSize ?? loaded) - loaded))})`}
          </button>
        )}
      </div>
    </details>
  );
}

// TodoChecklist renders the TodoWrite payload as a checkbox list:
//   - pending      : ☐  (empty checkbox)
//   - in_progress  : ☐ with ★ overlaid inside the box (the star *is*
//                    the check mark)
//   - completed    : ☑  with strikethrough on the task text
//
// Mirrors how Claude Code itself surfaces todo state — the active task's
// checkbox is filled with a star instead of the usual check, finished
// tasks are checked + struck through.
function TodoChecklist({ todos }: { todos: TodoItem[] }) {
  return (
    <ul className="mb-1.5 space-y-0.5 text-[11px] font-mono leading-tight">
      {todos.map((t, i) => {
        let textClasses: string;
        let box: ReactNode;
        switch (t.status) {
          case "in_progress":
            // Overlay ★ inside an empty ☐ so the star reads as the
            // checkbox's "check". The relative+absolute pairing keeps
            // them perfectly stacked across font sizes; the warning
            // color makes the active task pop out of the list.
            box = (
              <span className="relative inline-flex w-3 h-3 items-center justify-center select-none">
                <span className="absolute inset-0 text-fg-subtle leading-none">☐</span>
                <span className="relative text-warning-fg text-[9px] leading-none font-bold">
                  ★
                </span>
              </span>
            );
            textClasses = "text-fg-default font-semibold";
            break;
          case "completed":
            box = (
              <span className="select-none text-success-fg" aria-hidden>
                ☑
              </span>
            );
            textClasses = "text-fg-subtle line-through";
            break;
          default:
            box = (
              <span className="select-none text-fg-subtle" aria-hidden>
                ☐
              </span>
            );
            textClasses = "text-fg-default";
        }
        return (
          <li key={i} className="flex items-start gap-1.5 break-words">
            {box}
            <span className={`flex-1 ${textClasses}`}>
              {t.status === "in_progress" && t.activeForm ? t.activeForm : t.content}
            </span>
          </li>
        );
      })}
    </ul>
  );
}

// ToolFieldList renders the curated key/value pairs produced by
// formatToolCall() — path, pattern, command, etc. — as a compact grid
// at the top of each tool card. Always visible (unlike the raw
// input/output blocks) so the caller can see at a glance *what*
// arguments the agent invoked the tool with.
function ToolFieldList({ fields }: { fields: ToolField[] }) {
  return (
    <ul className="mb-1.5 grid grid-cols-[auto_minmax(0,1fr)] gap-x-2 gap-y-0.5 text-[10px]">
      {fields.map((f, i) => (
        <li key={`${f.label}-${i}`} className="contents">
          <span className="text-fg-subtle">{f.label}:</span>
          <span
            className={`text-fg-default break-all ${f.mono ? "font-mono" : ""}`}
            title={f.value}
          >
            {f.value}
          </span>
        </li>
      ))}
    </ul>
  );
}

function ToolCallCard({ call, runId }: { call: ToolCall; runId: string }) {
  const [showRaw, setShowRaw] = useState(false);
  // The tool input lives on event.data.input — already on the
  // ToolCall via `input` (stringified). Parsers expect a parsable
  // shape, so we feed them the raw object from rawData first and
  // fall back to the string if needed.
  const summary = useMemo(() => {
    const raw = call.rawData?.["input"];
    return formatToolCall(call.toolName, raw ?? call.input);
  }, [call.toolName, call.rawData, call.input]);
  return (
    <li
      className={`rounded border p-2 ${
        call.isError ? "border-danger/60 bg-danger-soft/30" : "border-border-default"
      }`}
    >
      <div className="flex items-center gap-2 mb-1">
        <span className="font-medium text-[11px]">{call.toolName}</span>
        <StatusBadge
          status={call.isError ? "failed" : "finished"}
          label={call.isError ? "error" : "ok"}
          showGlyph={false}
        />
        {call.duration !== undefined && (
          <span className="text-[10px] text-fg-subtle">
            {formatMs(call.duration)}
          </span>
        )}
        <span className="ml-auto text-[10px] font-mono text-fg-subtle">
          seq {call.seq}
        </span>
      </div>
      {summary.fields.length > 0 && <ToolFieldList fields={summary.fields} />}
      {summary.todos && summary.todos.length > 0 && (
        <TodoChecklist todos={summary.todos} />
      )}
      {call.errorMsg && (
        <div className="text-[10px] font-mono text-danger-fg whitespace-pre-wrap break-words mb-1">
          {call.errorMsg}
        </div>
      )}
      {call.input && (
        <ToolPayloadBlock
          label="raw input"
          value={call.input}
          runId={runId}
          toolUseID={call.inputRef}
          kind="input"
          totalSize={call.inputSize}
        />
      )}
      {call.output && (
        <ToolPayloadBlock
          label="output"
          value={call.output}
          runId={runId}
          toolUseID={call.outputRef}
          kind="output"
          totalSize={call.outputSize}
        />
      )}
      <button
        type="button"
        onClick={() => setShowRaw((v) => !v)}
        className="text-[10px] text-fg-subtle hover:text-fg-default"
      >
        {showRaw ? "hide raw" : "show raw"}
      </button>
      {showRaw && call.rawData && (
        <pre className="mt-1 text-[10px] font-mono whitespace-pre-wrap break-all bg-surface-1 rounded p-1.5 text-fg-subtle">
          {JSON.stringify(call.rawData, null, 2)}
        </pre>
      )}
    </li>
  );
}

// ---------------------------------------------------------------------------
// Pause tab
// ---------------------------------------------------------------------------

interface PauseInfo {
  questions: Record<string, unknown>;
  message?: string;
}

function usePauseInfo(matching: RunEvent[]): PauseInfo | null {
  return useMemo<PauseInfo | null>(() => {
    // Walk newest → oldest looking for the most recent
    // human_input_requested for this execution. The reducer in the
    // store flips status back to running on resume, so it's safe to
    // assume the latest pause request is the active one.
    for (let i = matching.length - 1; i >= 0; i--) {
      const e = matching[i]!;
      if (e.type === "human_input_requested" && e.data) {
        return {
          questions:
            (e.data["questions"] as Record<string, unknown> | undefined) ?? {},
          message:
            (e.data["message"] as string | undefined) ??
            (e.data["reason"] as string | undefined),
        };
      }
    }
    return null;
  }, [matching]);
}

// ---------------------------------------------------------------------------
// Events tab
// ---------------------------------------------------------------------------

function EventsTabContent({ events }: { events: RunEvent[] }) {
  const [search, setSearch] = useState("");
  const [activeTypes, setActiveTypes] = useState<Set<string>>(() => new Set());
  const [showRawData, setShowRawData] = useState(false);

  // Counts per type so the chip row can show occurrence numbers.
  const typeCounts = useMemo(() => {
    const m = new Map<string, number>();
    for (const e of events) m.set(e.type, (m.get(e.type) ?? 0) + 1);
    return m;
  }, [events]);

  const knownTypes = useMemo(
    () => Array.from(typeCounts.keys()).sort(),
    [typeCounts],
  );

  const filtered = useMemo(() => {
    const query = search.trim().toLowerCase();
    return events.filter((e) => {
      if (activeTypes.size > 0 && !activeTypes.has(e.type)) return false;
      if (!query) return true;
      // Search type, node_id, and stringified data so users can grep
      // for a substring (e.g. "rate_limit" or "tool_name=Bash").
      if (e.type.toLowerCase().includes(query)) return true;
      if (e.node_id?.toLowerCase().includes(query)) return true;
      if (e.data && JSON.stringify(e.data).toLowerCase().includes(query))
        return true;
      return false;
    });
  }, [events, activeTypes, search]);

  const toggleType = (t: string) => {
    setActiveTypes((prev) => {
      const next = new Set(prev);
      if (next.has(t)) next.delete(t);
      else next.add(t);
      return next;
    });
  };

  return (
    <div className="h-full flex flex-col">
      <div className="px-4 py-2 border-b border-border-default space-y-1.5">
        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search events…"
          size="sm"
          leadingIcon={<span className="text-[10px]">⌕</span>}
        />
        {knownTypes.length > 1 && (
          <div className="flex flex-wrap gap-1">
            {knownTypes.map((t) => {
              const isActive = activeTypes.has(t);
              return (
                <button
                  key={t}
                  type="button"
                  onClick={() => toggleType(t)}
                  className={`text-[10px] px-1.5 py-0.5 rounded border transition-colors ${
                    isActive
                      ? "bg-accent-soft border-accent text-fg-default"
                      : "bg-surface-1 border-border-default text-fg-subtle hover:text-fg-default"
                  }`}
                >
                  {t} <span className="text-fg-subtle">{typeCounts.get(t)}</span>
                </button>
              );
            })}
          </div>
        )}
        <div className="flex items-center gap-2">
          <span className="text-[10px] text-fg-subtle">
            {filtered.length} / {events.length} events
          </span>
          <button
            type="button"
            onClick={() => setShowRawData((v) => !v)}
            className="ml-auto text-[10px] text-fg-subtle hover:text-fg-default"
          >
            {showRawData ? "hide raw" : "show raw"}
          </button>
        </div>
      </div>
      <div className="flex-1 overflow-auto px-4 py-2">
        {filtered.length === 0 ? (
          <div className="text-fg-subtle">No events match.</div>
        ) : (
          <ul className="space-y-0.5 font-mono text-[10px]">
            {filtered.map((e) => (
              <li key={`${e.run_id}:${e.seq}`}>
                <div className="flex gap-2">
                  <span className="text-fg-subtle">
                    {e.seq.toString().padStart(4, "0")}
                  </span>
                  <span>{e.type}</span>
                </div>
                {showRawData && e.data && Object.keys(e.data).length > 0 && (
                  <pre className="ml-12 my-0.5 text-fg-subtle whitespace-pre-wrap break-all">
                    {JSON.stringify(e.data, null, 2)}
                  </pre>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function useExecutionEvents(events: RunEvent[], exec: ExecutionState | null) {
  return useMemo<RunEvent[]>(() => {
    if (!exec) return [];
    const out: RunEvent[] = [];
    const counts = new Map<string, number>();
    for (const e of events) {
      if (!e.node_id) continue;
      const iter = stepIteration(counts, e);
      if (
        (e.branch_id || "main") === exec.branch_id &&
        e.node_id === exec.ir_node_id &&
        iter === exec.loop_iteration
      ) {
        out.push(e);
      }
    }
    return out;
  }, [events, exec]);
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      onClick={async (e) => {
        e.stopPropagation();
        e.preventDefault();
        try {
          await navigator.clipboard.writeText(value);
          setCopied(true);
          setTimeout(() => setCopied(false), 1200);
        } catch {
          // ignore
        }
      }}
      className="text-[9px] text-fg-subtle hover:text-fg-default px-1"
    >
      {copied ? "copied" : "copy"}
    </button>
  );
}

function safeJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

// Re-export needed by EventLog (Phase 6 will own it).
export type { TabValue };
