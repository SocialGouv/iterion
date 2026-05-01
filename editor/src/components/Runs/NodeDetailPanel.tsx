import { useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";

import type { ArtifactSummary, ExecutionState, RunEvent } from "@/api/runs";
import { listArtifacts } from "@/api/runs";
import { Tabs } from "@/components/ui/Tabs";

import ArtifactDiff from "./ArtifactDiff";

interface Props {
  runId: string;
  // The .iter source path for this run; used to wire "Open in editor".
  filePath?: string;
  exec: ExecutionState | null;
  events: RunEvent[];
}

type TabValue = "trace" | "events" | "artifact" | "tools";

export default function NodeDetailPanel({ runId, filePath, exec, events }: Props) {
  const [, setLocation] = useLocation();
  const [artifactVersions, setArtifactVersions] = useState<ArtifactSummary[]>([]);
  const [showRawData, setShowRawData] = useState(false);
  const [activeTab, setActiveTab] = useState<TabValue | null>(null);

  // Load only the version index here; ArtifactDiff handles fetching the
  // body for each selected version on demand. This keeps the panel
  // light and lets the diff tab decide whether to render a single
  // version (no diff) or a real LCS diff between two of them.
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

  // Filter events that match this execution by replaying the
  // (branch, ir_node_id) iteration counter — same algorithm as the
  // backend reducer. Memoised because the events array can be large.
  const matching = useMemo<RunEvent[]>(() => {
    if (!exec) return [];
    const out: RunEvent[] = [];
    const counts = new Map<string, number>();
    for (const e of events) {
      if (!e.node_id) continue;
      const branch = e.branch_id || "main";
      const key = `${branch} ${e.node_id}`;
      let iter = counts.get(key);
      if (iter === undefined) iter = -1;
      if (e.type === "node_started") iter += 1;
      counts.set(key, iter);
      if (
        branch === exec.branch_id &&
        e.node_id === exec.ir_node_id &&
        iter === exec.loop_iteration
      ) {
        out.push(e);
      }
    }
    return out;
  }, [events, exec]);

  // Pull the LLM trace out of the matching events. We surface
  // system_prompt / user_message from llm_prompt and response_text
  // from the latest llm_step_finished — that's the round-trip an
  // operator usually wants to see.
  const llm = useMemo(() => {
    const trace: {
      systemPrompt?: string;
      userMessage?: string;
      response?: string;
      model?: string;
      inputTokens?: number;
      outputTokens?: number;
      finishReason?: string;
      toolCalls?: Array<{ name: string; input?: string }>;
    } = {};
    for (const e of matching) {
      if (e.type === "llm_prompt" && e.data) {
        trace.systemPrompt = (e.data["system_prompt"] as string) ?? trace.systemPrompt;
        trace.userMessage = (e.data["user_message"] as string) ?? trace.userMessage;
      } else if (e.type === "llm_request" && e.data) {
        trace.model = (e.data["model"] as string) ?? trace.model;
      } else if (e.type === "llm_step_finished" && e.data) {
        // Latest step wins — agent loops can have many steps;
        // we show the final response.
        trace.response = (e.data["response_text"] as string) ?? trace.response;
        trace.inputTokens = (e.data["input_tokens"] as number) ?? trace.inputTokens;
        trace.outputTokens = (e.data["output_tokens"] as number) ?? trace.outputTokens;
        trace.finishReason = (e.data["finish_reason"] as string) ?? trace.finishReason;
        const calls = e.data["tool_call_details"] as
          | Array<Record<string, unknown>>
          | undefined;
        if (calls) {
          trace.toolCalls = calls.map((c) => ({
            name: (c["tool_name"] as string) ?? "",
            input: c["input"] as string | undefined,
          }));
        }
      }
    }
    const present =
      trace.systemPrompt ||
      trace.userMessage ||
      trace.response ||
      trace.model ||
      trace.toolCalls?.length;
    return present ? trace : null;
  }, [matching]);

  // Tool call/error events isolated for the Tools tab. Distinct from
  // llm.toolCalls (which lives inside llm_step_finished payloads) — the
  // top-level tool events carry full input/output payloads with timing.
  const toolEvents = useMemo<RunEvent[]>(
    () => matching.filter((e) => e.type === "tool_called" || e.type === "tool_error"),
    [matching],
  );

  // Tab default depends on what's most useful for the node kind. Reset
  // it on exec change so navigating between executions surfaces the
  // primary view first; let the user override afterwards.
  useEffect(() => {
    if (!exec) {
      setActiveTab(null);
      return;
    }
    const kind = exec.kind;
    if (kind === "agent" || kind === "judge") setActiveTab("trace");
    else if (kind === "tool") setActiveTab("tools");
    else setActiveTab("events");
  }, [exec]);

  if (!exec) {
    return (
      <div className="h-full p-4 text-xs text-fg-subtle">
        Click an execution to see its events, prompt, response, artifact, and error trace.
      </div>
    );
  }

  const hasArtifact = artifactVersions.length > 0;

  // Tabs render eagerly (Radix mounts each <Content> only when active by
  // default, so we don't pay for hidden trees). Default tab gates the
  // initial mount; afterwards Radix preserves the tab tree across
  // switches.
  const tabItems = [
    { value: "trace" as TabValue, label: "Trace", disabled: !llm },
    { value: "events" as TabValue, label: `Events (${matching.length})` },
    {
      value: "artifact" as TabValue,
      label:
        artifactVersions.length > 1
          ? `Artifact (${artifactVersions.length})`
          : "Artifact",
      disabled: !hasArtifact,
    },
    {
      value: "tools" as TabValue,
      label: `Tools (${toolEvents.length})`,
      disabled: toolEvents.length === 0,
    },
  ];

  return (
    <div className="h-full flex flex-col text-xs">
      <div className="px-4 pt-3 pb-2 border-b border-border-default">
        <h2 className="font-bold text-sm mb-1 truncate">{exec.ir_node_id}</h2>
        <div className="text-fg-subtle text-[10px] space-y-0.5">
          <div>
            execution: <span className="font-mono">{exec.execution_id}</span>
          </div>
          <div>
            branch: {exec.branch_id} · iteration: {exec.loop_iteration}
          </div>
          {exec.kind && <div>kind: {exec.kind}</div>}
          <div>status: {exec.status}</div>
          {exec.started_at && (
            <div>started: {new Date(exec.started_at).toLocaleTimeString()}</div>
          )}
          {exec.finished_at && (
            <div>finished: {new Date(exec.finished_at).toLocaleTimeString()}</div>
          )}
        </div>

        {filePath && (
          <button
            type="button"
            onClick={() =>
              setLocation(
                `/?file=${encodeURIComponent(filePath)}&node=${encodeURIComponent(exec.ir_node_id)}&from=${encodeURIComponent(runId)}`,
              )
            }
            className="mt-2 text-[11px] px-2 py-1 rounded bg-surface-2 hover:bg-surface-3 text-fg-default"
            title="Open the .iter source in the editor"
          >
            ↗ Open {exec.ir_node_id} in editor
          </button>
        )}

        {exec.error && (
          <div className="mt-2 px-2 py-1.5 rounded bg-danger-soft text-danger-fg">
            <div className="font-medium mb-0.5">Error</div>
            <div className="font-mono whitespace-pre-wrap">{exec.error}</div>
          </div>
        )}
      </div>

      <Tabs
        value={activeTab ?? "events"}
        onValueChange={(v) => setActiveTab(v as TabValue)}
        items={tabItems}
        variant="underline"
        listClassName="px-3"
        className="flex-1 min-h-0"
        panels={{
          trace: (
            <div className="overflow-auto px-4 py-3 h-full">
              {!llm ? (
                <p className="text-fg-subtle">No LLM activity recorded for this execution.</p>
              ) : (
                <LLMTraceView trace={llm} />
              )}
            </div>
          ),
          events: (
            <div className="overflow-auto px-4 py-3 h-full">
              <div className="mb-2">
                <button
                  type="button"
                  onClick={() => setShowRawData((v) => !v)}
                  className="text-[10px] text-fg-subtle hover:text-fg-default"
                >
                  {showRawData ? "hide raw data" : "show raw data"}
                </button>
              </div>
              {matching.length === 0 ? (
                <div className="text-fg-subtle">No events for this execution.</div>
              ) : (
                <ul className="space-y-0.5 font-mono text-[10px]">
                  {matching.map((e) => (
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
          tools: (
            <div className="overflow-auto px-4 py-3 h-full">
              {toolEvents.length === 0 ? (
                <div className="text-fg-subtle">No tool calls for this execution.</div>
              ) : (
                <ul className="space-y-2">
                  {toolEvents.map((e) => (
                    <li
                      key={`${e.run_id}:${e.seq}`}
                      className={`rounded border ${
                        e.type === "tool_error" ? "border-danger" : "border-border-default"
                      } p-2`}
                    >
                      <div className="flex justify-between text-[10px] text-fg-subtle font-mono mb-1">
                        <span>{e.type}</span>
                        <span>seq {e.seq}</span>
                      </div>
                      <div className="font-medium text-[11px]">
                        {(e.data?.["tool_name"] as string) ??
                          (e.data?.["tool"] as string) ??
                          "unknown tool"}
                      </div>
                      {e.data && Object.keys(e.data).length > 0 && (
                        <pre className="mt-1 text-[10px] font-mono whitespace-pre-wrap break-all text-fg-subtle">
                          {JSON.stringify(e.data, null, 2)}
                        </pre>
                      )}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ),
        }}
      />
    </div>
  );
}

function LLMTraceView({
  trace,
}: {
  trace: {
    systemPrompt?: string;
    userMessage?: string;
    response?: string;
    model?: string;
    inputTokens?: number;
    outputTokens?: number;
    finishReason?: string;
    toolCalls?: Array<{ name: string; input?: string }>;
  };
}) {
  return (
    <>
      {(trace.model || trace.inputTokens !== undefined) && (
        <div className="text-[10px] text-fg-subtle mb-2">
          {trace.model && (
            <span>
              model: <span className="font-mono">{trace.model}</span>
            </span>
          )}
          {trace.inputTokens !== undefined && (
            <span className="ml-2">
              in: {trace.inputTokens} · out: {trace.outputTokens ?? 0} tok
            </span>
          )}
          {trace.finishReason && (
            <span className="ml-2">finish: {trace.finishReason}</span>
          )}
        </div>
      )}
      {trace.systemPrompt && <TraceBlock title="system prompt" body={trace.systemPrompt} />}
      {trace.userMessage && <TraceBlock title="user message" body={trace.userMessage} />}
      {trace.response && <TraceBlock title="response" body={trace.response} />}
      {trace.toolCalls && trace.toolCalls.length > 0 && (
        <details className="mb-1 rounded border border-border-default">
          <summary className="px-2 py-1 cursor-pointer text-fg-muted bg-surface-2 rounded-t">
            tool calls ({trace.toolCalls.length})
          </summary>
          <ul className="p-2 text-[10px] font-mono space-y-1">
            {trace.toolCalls.map((c, i) => (
              <li key={i}>
                <span className="text-fg-default">{c.name}</span>
                {c.input && (
                  <pre className="text-fg-subtle whitespace-pre-wrap">{c.input}</pre>
                )}
              </li>
            ))}
          </ul>
        </details>
      )}
    </>
  );
}

function TraceBlock({ title, body }: { title: string; body: string }) {
  return (
    <details className="mb-1 rounded border border-border-default" open>
      <summary className="px-2 py-1 cursor-pointer text-fg-muted bg-surface-2 rounded-t">
        {title}
      </summary>
      <pre className="p-2 text-[10px] font-mono whitespace-pre-wrap max-h-48 overflow-auto">
        {body}
      </pre>
    </details>
  );
}
