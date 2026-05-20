import { memo, useMemo } from "react";

import type { NodeOutputMessage } from "@/lib/runChat/types";

import MarkdownText from "./MarkdownText";

interface Props {
  message: NodeOutputMessage;
}

// NodeOutputCard renders a node's structured output as a chat card.
// Strategy:
//   - Filter `_*` metadata keys (runtime adds _model, _cost_usd, etc.).
//   - If the surviving record has a single string field, render that
//     verbatim as markdown — typical for free-form agent answers.
//   - Otherwise render each field as a labelled section:
//     bold heading + markdown body for strings, fenced JSON for
//     objects / arrays / non-strings.
function NodeOutputCardImpl({ message }: Props) {
  // `message.output` is frozen at node_finished time and never mutates,
  // so the derived markdown is stable. memo() + useMemo together skip
  // the JSON.stringify pass on every parent re-render (every WS tick).
  const md = useMemo(() => prettyMd(stripMeta(message.output)), [message.output]);
  if (!md) return null;
  return (
    <div className="ml-5 mt-1 rounded-md border border-border-subtle bg-surface-1 px-3 py-2 space-y-1">
      <div className="flex items-baseline gap-2 text-[10px] font-mono text-fg-subtle">
        <span>{message.nodeId}</span>
        {message.iteration > 0 && (
          <span className="text-fg-muted">iter {message.iteration}</span>
        )}
      </div>
      <MarkdownText value={md} />
    </div>
  );
}

const NodeOutputCard = memo(NodeOutputCardImpl);
export default NodeOutputCard;

// stripMeta removes the runtime's `_*` annotation keys (added by the
// executor — _backend, _cost_usd, _model, _tokens, _context_*).
// Operators never want to read these in the chat; they live in the
// detail panel and metrics. Undefined-valued keys are also dropped:
// some workflows declare optional fields the runtime leaves unset.
function stripMeta(v: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, val] of Object.entries(v)) {
    if (!k.startsWith("_") && val !== undefined) out[k] = val;
  }
  return out;
}

function prettyMd(value: Record<string, unknown>): string {
  const entries = Object.entries(value);
  if (entries.length === 0) return "";
  // Single string field → render verbatim. Typical for {text: "..."}
  // or {answer: "..."} shapes; the operator wants the prose, not the
  // field name.
  if (entries.length === 1) {
    const [, v] = entries[0]!;
    if (typeof v === "string") return v;
  }
  // Multi-field: section per key.
  const parts: string[] = [];
  for (const [k, v] of entries) {
    parts.push(`#### ${k}`);
    parts.push(renderValue(v));
  }
  return parts.join("\n\n");
}

function renderValue(v: unknown): string {
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  if (v === null) return "_(null)_";
  if (Array.isArray(v) && v.every((x) => typeof x === "string")) {
    return (v as string[]).map((s) => `- ${s}`).join("\n");
  }
  // Objects / arrays / mixed → fenced JSON for legibility.
  try {
    return "```json\n" + JSON.stringify(v, null, 2) + "\n```";
  } catch {
    return String(v);
  }
}
