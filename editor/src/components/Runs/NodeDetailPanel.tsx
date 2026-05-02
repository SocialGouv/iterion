import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useLocation } from "wouter";
import { ChevronRightIcon } from "@radix-ui/react-icons";

import type { ArtifactSummary, ExecutionState, RunEvent } from "@/api/runs";
import { listArtifacts } from "@/api/runs";
import { IconButton, Input, StatusBadge, Tabs } from "@/components/ui";
import { stepIteration } from "@/lib/eventIter";
import { formatDurationBetween, formatMs } from "@/lib/format";

import ArtifactDiff from "./ArtifactDiff";
import PauseForm from "./PauseForm";

interface Props {
  runId: string;
  // The .iter source path for this run; used to wire "Open in editor".
  filePath?: string;
  exec: ExecutionState | null;
  events: RunEvent[];
  // followLive == true → the parent is auto-tracking the running
  // execution; clicking the toggle off pins the panel on the current
  // exec. Clicking it on again re-engages auto-tracking, which the
  // parent implements by clearing the manual pin in handleToggle.
  followLive?: boolean;
  onToggleFollowLive?: () => void;
  onCollapse?: () => void;
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

type TabValue = "pause" | "trace" | "tools" | "artifact" | "events";

export default function NodeDetailPanel({
  runId,
  filePath,
  exec,
  events,
  followLive,
  onToggleFollowLive,
  onCollapse,
}: Props) {
  const [artifactVersions, setArtifactVersions] = useState<ArtifactSummary[]>([]);
  const [activeTab, setActiveTab] = useState<TabValue | null>(null);

  // Load only the version index here; ArtifactDiff handles fetching the
  // body for each selected version on demand.
  useEffect(() => {
    setArtifactVersions([]);
    if (!exec) return;
    let cancelled = false;
    listArtifacts(runId, exec.ir_node_id)
      .then((summaries) => {
        if (cancelled) return;
        setArtifactVersions(summaries);
      })
      .catch(() => {
        // Artifacts are best-effort — silent fall-through.
      });
    return () => {
      cancelled = true;
    };
  }, [runId, exec]);

  const matching = useExecutionEvents(events, exec);
  const llmSteps = useLLMSteps(matching);
  const toolCalls = useToolCalls(matching);
  const pause = usePauseInfo(matching);

  // Tab default depends on what's most useful for the node kind. Reset
  // it on exec change so navigating between executions surfaces the
  // primary view first; let the user override afterwards.
  useEffect(() => {
    if (!exec) {
      setActiveTab(null);
      return;
    }
    if (exec.status === "paused_waiting_human") {
      setActiveTab("pause");
      return;
    }
    const kind = exec.kind;
    if (kind === "agent" || kind === "judge") setActiveTab("trace");
    else if (kind === "tool") setActiveTab("tools");
    else setActiveTab("events");
  }, [exec]);

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
  ];

  return (
    <div className="relative h-full flex flex-col text-xs">
      <CollapseButton onCollapse={onCollapse} />
      <DetailHeader
        runId={runId}
        filePath={filePath}
        exec={exec}
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
                <ToolCallList calls={toolCalls} />
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
  events,
  followLive,
  onToggleFollowLive,
}: {
  runId: string;
  filePath?: string;
  exec: ExecutionState;
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
  const { costUsd, tokens, model } = useMemo(() => {
    let costUsd = 0;
    let tokens = 0;
    let model = "";
    for (const e of events) {
      if (e.type !== "node_finished" || !e.data) continue;
      const c = e.data["_cost_usd"];
      if (typeof c === "number") costUsd += c;
      const t = e.data["_tokens"];
      if (typeof t === "number") tokens += t;
      const out = e.data["output"] as Record<string, unknown> | undefined;
      const m = out?.["_model"];
      if (typeof m === "string" && m && !model) model = m;
    }
    return { costUsd, tokens, model };
  }, [events]);
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
            <span>iter: {exec.loop_iteration + 1}</span>
            {duration && <span>duration: {duration}</span>}
            {tokens > 0 && <span>tokens: {tokens.toLocaleString()}</span>}
            {costUsd > 0 && (
              <span title={`$${costUsd.toFixed(6)}`}>
                cost: ${costUsd.toFixed(4)}
              </span>
            )}
            {model && <span className="font-mono">{model}</span>}
          </div>
        </div>
        {filePath && (
          <IconButton
            label="Open in editor"
            tooltip="Open this node in the editor"
            size="sm"
            variant="ghost"
            onClick={() =>
              setLocation(
                `/?file=${encodeURIComponent(filePath)}&node=${encodeURIComponent(
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
  input?: string;
  output?: string;
  errorMsg?: string;
}

function useToolCalls(matching: RunEvent[]): ToolCall[] {
  return useMemo<ToolCall[]>(() => {
    return matching
      .filter((e) => e.type === "tool_called" || e.type === "tool_error")
      .map((e) => {
        const data = e.data ?? {};
        const toolName =
          (data["tool_name"] as string) ?? (data["tool"] as string) ?? "unknown";
        const isError = e.type === "tool_error";
        return {
          seq: e.seq,
          toolName,
          isError,
          duration:
            typeof data["duration_ms"] === "number"
              ? (data["duration_ms"] as number)
              : undefined,
          rawData: data,
          input:
            typeof data["input"] === "string"
              ? (data["input"] as string)
              : data["input"] !== undefined
              ? safeJSON(data["input"])
              : undefined,
          output:
            typeof data["output"] === "string"
              ? (data["output"] as string)
              : data["output"] !== undefined
              ? safeJSON(data["output"])
              : undefined,
          errorMsg: isError
            ? (data["error"] as string) ?? (data["message"] as string)
            : undefined,
        };
      });
  }, [matching]);
}

function ToolCallList({ calls }: { calls: ToolCall[] }) {
  return (
    <ul className="space-y-2">
      {calls.map((c) => (
        <ToolCallCard key={c.seq} call={c} />
      ))}
    </ul>
  );
}

function ToolCallCard({ call }: { call: ToolCall }) {
  const [showRaw, setShowRaw] = useState(false);
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
      {call.errorMsg && (
        <div className="text-[10px] font-mono text-danger-fg whitespace-pre-wrap break-words mb-1">
          {call.errorMsg}
        </div>
      )}
      {call.input && (
        <details className="mb-1">
          <summary className="cursor-pointer text-[10px] text-fg-muted px-1 py-0.5 rounded hover:bg-surface-2 flex items-center justify-between">
            <span>input</span>
            <CopyButton value={call.input} />
          </summary>
          <pre className="mt-1 text-[10px] font-mono whitespace-pre-wrap break-words bg-surface-1 rounded p-1.5 max-h-40 overflow-auto">
            {call.input}
          </pre>
        </details>
      )}
      {call.output && (
        <details className="mb-1">
          <summary className="cursor-pointer text-[10px] text-fg-muted px-1 py-0.5 rounded hover:bg-surface-2 flex items-center justify-between">
            <span>output</span>
            <CopyButton value={call.output} />
          </summary>
          <pre className="mt-1 text-[10px] font-mono whitespace-pre-wrap break-words bg-surface-1 rounded p-1.5 max-h-40 overflow-auto">
            {call.output}
          </pre>
        </details>
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
