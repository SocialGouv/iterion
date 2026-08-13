import { useMemo, useState } from "react";

import type { WireSchemaField } from "@/api/runs";
import { CopyButton } from "@/components/ui";
import {
  formatJSONValue,
  gateInboundItems,
  type GateInboundItem,
} from "@/lib/gateInbound";

import GateInboundFile from "./GateInboundFile";
import {
  COLLAPSE_AFTER_LINES,
  COLLAPSED_MAX_HEIGHT,
  MARKDOWN_PARSE_BUDGET,
  Toggle,
} from "./gateInboundFold";
import MarkdownText from "./MarkdownText";

interface Props {
  runId: string;
  /** The paused node's resolved INBOUND data (store.Interaction.Questions). */
  questions: Record<string, unknown> | null | undefined;
  /** The node's declared `input_schema`, when it has one. */
  inputFields?: WireSchemaField[] | null;
  /**
   * Keys the node's `instructions:` prompt interpolates. Paired with
   * `instructionsText` to skip a value that is already on screen — 13
   * gates across 8 shipped catalog bots inline their input this way
   * (Nexie's `chat_instructions` is literally `{{input.reply}}`).
   */
  instructionInputs?: string[] | null;
  /** The instructions text actually rendered above the form, if any. */
  instructionsText?: string | null;
}

/**
 * What the operator is being asked to validate, rendered above the answer
 * form.
 *
 * The pause already carries this data — the engine resolves the node's
 * incoming `with {}` mappings and stores them on the interaction — but
 * the form is driven by the node's OUTPUT schema, so until now the
 * payload reached the browser and was dropped on the floor (iterion#332).
 * A gate that already receives `with {plan: "{{outputs.plan.body}}"}`
 * therefore starts showing the plan with no authoring change at all.
 *
 * Long payloads start collapsed: a gate whose whole question is in
 * `instructions:` must not be pushed off-screen by a diff it also
 * happens to receive.
 */
export default function GateInboundPayload({
  runId,
  questions,
  inputFields,
  instructionInputs,
  instructionsText,
}: Props) {
  const items = useMemo(
    () => gateInboundItems(questions, inputFields, { instructionInputs, instructionsText }),
    [questions, inputFields, instructionInputs, instructionsText],
  );
  if (items.length === 0) return null;

  return (
    <section
      className="rounded-md border border-border-subtle bg-surface-1/60 px-2 py-1.5"
      aria-label="Review context"
    >
      <h4 className="mb-1 text-micro font-medium uppercase tracking-wide text-fg-subtle">
        What you're reviewing
      </h4>
      <dl className="space-y-1.5">
        {items.map((item) => (
          <div key={item.key} className="min-w-0">
            <dt className="text-micro font-medium text-fg-muted" title={item.key}>
              {humanizeKey(item.key)}
            </dt>
            <dd className="min-w-0 text-body text-fg-default">
              <GateInboundValue runId={runId} item={item} />
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

function GateInboundValue({ runId, item }: { runId: string; item: GateInboundItem }) {
  switch (item.kind) {
    case "file":
      // Keyed on the file's identity: a payload that swaps one
      // attachment for another (a re-answered gate producing a new
      // upload) must remount the fetch, not keep showing the old blob.
      return item.file ? (
        <GateInboundFile
          key={item.file.attachment ?? item.file.path ?? item.key}
          runId={runId}
          file={item.file}
        />
      ) : null;
    case "json":
      return <JSONBlock value={item.value} />;
    case "scalar":
      return <span className="break-words">{item.text}</span>;
    case "markdown":
      return <CollapsibleText text={item.text ?? ""} />;
  }
}

function CollapsibleText({ text }: { text: string }) {
  const lines = text.split("\n");
  const long = lines.length > COLLAPSE_AFTER_LINES;
  const [open, setOpen] = useState(!long);
  const folded = long && !open;
  const heavy = text.length > MARKDOWN_PARSE_BUDGET;
  return (
    <div className="min-w-0">
      {/*
        Folded with a height clamp, NOT by slicing the source: markdown
        has multi-line constructs, and cutting at line 12 inside a fenced
        code block would render an unterminated fence — swallowing the
        rest of the payload into a code block that never closes.
      */}
      <div
        className={folded ? "relative overflow-hidden" : undefined}
        style={folded ? { maxHeight: COLLAPSED_MAX_HEIGHT } : undefined}
      >
        {folded && heavy ? (
          <pre className="whitespace-pre-wrap break-words font-mono text-micro leading-relaxed">
            {text.slice(0, MARKDOWN_PARSE_BUDGET)}
          </pre>
        ) : (
          <MarkdownText value={text} size="sm" />
        )}
        {folded && (
          <div
            aria-hidden="true"
            className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-gradient-to-b from-transparent to-surface-1"
          />
        )}
      </div>
      {long && (
        <Toggle
          open={open}
          onToggle={() => setOpen((v) => !v)}
          closedLabel={`Show all ${lines.length} lines`}
        />
      )}
    </div>
  );
}

function JSONBlock({ value }: { value: unknown }) {
  const text = useMemo(() => formatJSONValue(value), [value]);
  const lines = text.split("\n");
  const long = lines.length > COLLAPSE_AFTER_LINES;
  const [open, setOpen] = useState(!long);
  const shown = open ? text : lines.slice(0, COLLAPSE_AFTER_LINES).join("\n");
  return (
    <div className="min-w-0 space-y-0.5">
      <div className="flex items-start gap-1">
        <pre className="min-w-0 flex-1 overflow-x-auto whitespace-pre-wrap break-words rounded bg-surface-0 p-1.5 font-mono text-micro leading-relaxed">
          {shown}
        </pre>
        <CopyButton value={text} variant="icon" label="Copy JSON" copiedLabel="Copied" />
      </div>
      {long && (
        <Toggle
          open={open}
          onToggle={() => setOpen((v) => !v)}
          closedLabel={`Show all ${lines.length} lines`}
        />
      )}
    </div>
  );
}

// humanizeKey turns a `with {}` mapping key into a label:
// "review_notes" → "Review notes". The raw key stays in the title
// attribute so an operator can still map it back to the .bot.
function humanizeKey(key: string): string {
  const words = key.replace(/[_-]+/g, " ").trim();
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : key;
}
