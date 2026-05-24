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
    // Single field that itself wraps a known shape (e.g. a compute
    // node that emits `{roadmap: {...}}`) → unwrap so the operator
    // doesn't see the field name as a heading on top of the actual
    // content.
    if (isPlainObject(v)) return prettyMd(stripMeta(v));
  }
  // Multi-field: section per key.
  const parts: string[] = [];
  for (const [k, v] of entries) {
    parts.push(`#### ${humanizeKey(k)}`);
    parts.push(renderValue(v));
  }
  return parts.join("\n\n");
}

function renderValue(v: unknown): string {
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  if (v === null) return "_(null)_";
  if (Array.isArray(v)) {
    if (v.length === 0) return "_(empty)_";
    if (v.every((x) => typeof x === "string")) {
      return (v as string[]).map((s) => `- ${s}`).join("\n");
    }
    // Array of "card-shaped" objects (roadmap items, findings,
    // proposals, …) → render each as a labelled card so the operator
    // reads prose instead of fenced JSON.
    if (v.every(isCardShaped)) {
      return v.map((it, idx) => renderCard(it as Record<string, unknown>, idx + 1)).join("\n\n");
    }
  }
  // Single object with a `title` + `body`-ish shape → one card.
  if (isCardShaped(v)) {
    return renderCard(v as Record<string, unknown>);
  }
  // Objects / arrays / mixed → fenced JSON for legibility.
  try {
    return "```json\n" + JSON.stringify(v, null, 2) + "\n```";
  } catch {
    return String(v);
  }
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return (
    typeof v === "object" &&
    v !== null &&
    !Array.isArray(v) &&
    Object.getPrototypeOf(v) === Object.prototype
  );
}

// isCardShaped picks objects that look like a readable item rather
// than free-form structured data: a string `title` plus at least one
// of `body` / `description` / `summary`. Used to decide whether to
// render as a card vs. fall through to JSON.
function isCardShaped(v: unknown): boolean {
  if (!isPlainObject(v)) return false;
  if (typeof v.title !== "string" || v.title.length === 0) return false;
  return (
    typeof v.body === "string" ||
    typeof v.description === "string" ||
    typeof v.summary === "string"
  );
}

function renderCard(it: Record<string, unknown>, index?: number): string {
  const title = typeof it.title === "string" ? it.title : "(untitled)";
  const body =
    typeof it.body === "string"
      ? it.body
      : typeof it.description === "string"
        ? it.description
        : typeof it.summary === "string"
          ? (it.summary as string)
          : "";
  const assignee = typeof it.assignee === "string" ? it.assignee.trim() : "";
  const args = isPlainObject(it.args) ? (it.args as Record<string, unknown>) : null;
  const hasArgs = args !== null && Object.keys(args).length > 0;
  const prefix = index !== undefined ? `${index}. ` : "";
  const lines: string[] = [];
  // Use bold rather than another heading level — the parent block
  // already nested us under `#### {key}` and we want the title to
  // read like a list item, not a competing section header.
  const badge = assignee
    ? ` &nbsp;·&nbsp; \`${assignee}\``
    : " &nbsp;·&nbsp; _(no bot)_";
  lines.push(`**${prefix}${title}**${badge}`);
  if (body) lines.push(body);
  if (hasArgs) {
    lines.push("_Args:_");
    lines.push("```json\n" + JSON.stringify(args, null, 2) + "\n```");
  }
  return lines.join("\n\n");
}

// humanizeKey turns snake_case / camelCase field names into a title
// the operator reads as English (e.g. `next_action` → "Next action").
function humanizeKey(k: string): string {
  const spaced = k.replace(/_/g, " ").replace(/([a-z])([A-Z])/g, "$1 $2");
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}
