// LLM trace tab for NodeDetailPanel: folds a node's llm_prompt / llm_request /
// llm_step_finished events into per-step cards (system/user/response blocks +
// tool calls). Extracted from NodeDetailPanel.tsx to keep that file focused.
import { useMemo, useState } from "react";

import type { RunEvent } from "@/api/runs";
import { ExpandableValue } from "@/components/ui";
import { isRecord } from "@/lib/isRecord";

// llm_prompt / llm_request / llm_step_finished are pass-through events
// (data: Record<string, unknown>) read here via bracket access + local
// validation, mirroring pkg/cli/inspect_node.go's stringField reader.
const str = (v: unknown): string | undefined =>
  typeof v === "string" ? v : undefined;
const num = (v: unknown): number | undefined =>
  typeof v === "number" ? v : undefined;

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

export function useLLMSteps(matching: RunEvent[]): LLMStep[] {
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
          systemPrompt: str(e.data["system_prompt"]),
          userMessage: str(e.data["user_message"]),
          model: lastModel,
          pending: true,
        };
      } else if (e.type === "llm_request" && e.data) {
        const model = str(e.data["model"]);
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
        current.response = str(e.data["response_text"]) ?? current.response;
        current.inputTokens = num(e.data["input_tokens"]) ?? current.inputTokens;
        current.outputTokens =
          num(e.data["output_tokens"]) ?? current.outputTokens;
        current.finishReason =
          str(e.data["finish_reason"]) ?? current.finishReason;
        const calls = e.data["tool_call_details"];
        if (Array.isArray(calls)) {
          current.toolCalls = calls.filter(isRecord).map((c) => ({
            name: str(c["tool_name"]) ?? "",
            input: str(c["input"]),
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

export function LLMTraceView({ steps }: { steps: LLMStep[] }) {
  const totalIn = steps.reduce((s, x) => s + (x.inputTokens ?? 0), 0);
  const totalOut = steps.reduce((s, x) => s + (x.outputTokens ?? 0), 0);
  return (
    <div className="space-y-2">
      <div className="text-caption text-fg-subtle flex flex-wrap gap-x-3 gap-y-0.5">
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
        aria-expanded={open}
        className="w-full flex items-center gap-2 px-2 py-1.5 text-left hover:bg-surface-2 rounded-t"
      >
        <span className="text-caption font-mono text-fg-subtle">
          step {index + 1}
        </span>
        {step.pending ? (
          <span className="text-caption text-info-fg animate-pulse">
            ⏳ in flight
          </span>
        ) : (
          <span className="text-caption text-fg-muted">{summary}</span>
        )}
        {step.model && (
          <span className="text-caption font-mono text-fg-subtle truncate">
            {step.model}
          </span>
        )}
        {(step.inputTokens !== undefined || step.outputTokens !== undefined) && (
          <span className="text-caption text-fg-subtle">
            {step.inputTokens ?? 0}↑/{step.outputTokens ?? 0}↓
          </span>
        )}
        <span className="ml-auto text-fg-subtle text-caption">{open ? "▾" : "▸"}</span>
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
              <ul className="p-2 text-caption font-mono space-y-1">
                {step.toolCalls.map((c, i) => (
                  <li key={i}>
                    <span className="text-fg-default">{c.name}</span>
                    {c.input && (
                      // Tool inputs are almost always JSON — the same
                      // expand/copy/pretty affordances pay off here too.
                      <ExpandableValue
                        value={c.input}
                        label={`${c.name} input`}
                        variant="bare"
                      />
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

// TraceBlock is one system-prompt / user-message / response section. The body
// goes through the shared ExpandableValue so a long prompt behaves here
// exactly as it does in the pipeline card drawer: collapsed to a preview,
// expandable IN PLACE, copyable, and pretty-printable when it holds JSON
// (LLM responses very often do). It replaces a `max-h-60 overflow-auto` box
// that could only ever show ~15 lines.
//
// Exported for unit tests, which is why it is not inlined above.
export function TraceBlock({
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
      <summary className="px-2 py-1 cursor-pointer text-fg-muted bg-surface-2 rounded-t">
        {title}
      </summary>
      {/* Copy lives on the value, not the summary: it must follow the
          raw/pretty toggle, and a summary button would copy the other form. */}
      <ExpandableValue
        value={body}
        label={title}
        variant="bare"
        collapsedMaxHeight="15rem"
        className="p-2"
      />
    </details>
  );
}
