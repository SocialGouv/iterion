import {
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import type { CodemodeReport, RunEvent } from "@/api/runs";
import { fetchToolBlob } from "@/api/runs";
import { formatBytes, formatMs } from "@/lib/format";
import { errorMessage } from "@/lib/errorHints";
import { CopyButton, StatusBadge, Table, TBody, Td, Th, THead, Tr } from "@/components/ui";

import { formatToolCall, type ToolField, type TodoItem } from "../toolFormatters";

import { groupToolCalls, isRecord, nestedItems, type ToolCall } from "./toolCallModel";

// ---------------------------------------------------------------------------
// Tools tab
// ---------------------------------------------------------------------------

export function useToolCalls(matching: RunEvent[]): ToolCall[] {
  return useMemo(() => groupToolCalls(matching), [matching]);
}

export function ToolCallList({ calls, runId }: { calls: ToolCall[]; runId: string }) {
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
export function ToolPayloadBlock({
  label,
  source,
  value,
  runId,
  toolUseID,
  kind,
  totalSize,
}: {
  label: string;
  // The row this payload belongs to ("Bash seq 12"), for the copy button's
  // accessible name: the same two blocks repeat for every tool call on the
  // surface, so "copy" alone names dozens of buttons identically.
  source: string;
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
  // retention of MBs of stdout in the studio heap.
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
  // One string for the eye and for the name — see the button below. Only
  // computed where it renders: with nothing more to fetch the subtraction
  // below goes negative and formats a figure nobody reads.
  const moreLabel = !hasMore
    ? ""
    : loading
      ? "loading…"
      : `load more (+${formatBytes(Math.min(FETCH_CHUNK_BYTES, (totalSize ?? loaded) - loaded))})`;

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
      setFetchErr(errorMessage(err));
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
    // The copy control sits BESIDE the disclosure rather than inside its
    // <summary>: a <summary> is itself a control, and a button nested in one
    // is nested-interactive (axe) — which is also why copying had to stop the
    // click from toggling the block.
    <div className="mb-1 flex items-start gap-1">
      <details
        className="min-w-0 flex-1"
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
        <summary className="cursor-pointer text-caption text-fg-muted px-1 py-0.5 rounded hover:bg-surface-2 flex items-center">
          <span>
            {label}
            {totalSize !== undefined && totalSize > value.length && (
              <span className="ml-1 text-fg-subtle">
                ({formatBytes(loaded)} / {formatBytes(totalSize)})
              </span>
            )}
          </span>
        </summary>
        <pre
          ref={preRef}
          onScroll={handleScroll}
          className={`mt-1 text-caption font-mono whitespace-pre-wrap break-words bg-surface-1 rounded p-1.5 ${
            expanded ? "max-h-[60vh] overflow-auto" : "max-h-40 overflow-auto"
          }`}
        >
          {fullValue}
        </pre>
        {fetchErr && (
          <div className="mt-1 text-caption text-danger-fg px-1">
            fetch error: {fetchErr}
          </div>
        )}
        <div className="mt-1 flex gap-1.5 items-center">
          {(overflow || expanded) && (
            <button
              type="button"
              onClick={() => setExpanded((v) => !v)}
              aria-expanded={expanded}
              // Every block on the surface repeats this control: the name
              // carries the payload and the row it belongs to.
              aria-label={`${expanded ? "collapse" : "expand"} ${label} of ${source}`}
              className="text-caption text-fg-subtle hover:text-fg-default px-1 py-0.5 rounded hover:bg-surface-2"
            >
              {expanded ? "collapse" : "expand"}
            </button>
          )}
          {hasMore && (
            <button
              type="button"
              onClick={() => void loadMore()}
              disabled={loading}
              // The name is built FROM the rendered string, byte figure and
              // all: a name that drops what the eye reads fails SC 2.5.3 and
              // takes the size with it.
              aria-label={`${moreLabel} of ${label} of ${source}`}
              className="text-caption text-fg-subtle hover:text-fg-default px-1 py-0.5 rounded hover:bg-surface-2 disabled:opacity-50"
            >
              {moreLabel}
            </button>
          )}
        </div>
      </details>
      <CopyButton value={fullValue} describes={`${label} of ${source}`} />
    </div>
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
    <ul className="mb-1.5 space-y-0.5 text-micro font-mono leading-tight">
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
    <ul className="mb-1.5 grid grid-cols-[auto_minmax(0,1fr)] gap-x-2 gap-y-0.5 text-caption">
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

// The codemode tool's name (pkg/backend/model ScriptHostToolName).
const CODEMODE_TOOL = "codemode";

// The marker object the sandbox relay sends in place of an input over its
// 1 MiB field budget (pkg/backend/model relayClampJSON).
const RELAY_OMITTED_KEY = "_iterion_sandbox_relay_omitted_bytes";

type ProgramPayload =
  | { kind: "source"; text: string }
  | { kind: "omitted"; bytes: number }
  | { kind: "raw" };

// programOf reads a codemode call's input — the program — for display. A
// whole inline input yields the source text; the relay's omitted marker
// yields its size; a 4 KB head preview of a larger input is not a JSON
// document, and is shown as it is stored, with load more.
function programOf(call: ToolCall): ProgramPayload | undefined {
  const raw = call.rawData?.["input"];
  let parsed: unknown = raw;
  if (typeof raw === "string") {
    try {
      parsed = JSON.parse(raw);
    } catch {
      // Not a whole JSON document: the stored preview, shown raw.
      return call.input ? { kind: "raw" } : undefined;
    }
  }
  if (isRecord(parsed)) {
    const src = parsed["source"];
    if (typeof src === "string") return { kind: "source", text: src };
    const omitted = parsed[RELAY_OMITTED_KEY];
    if (typeof omitted === "number") return { kind: "omitted", bytes: omitted };
  }
  return call.input ? { kind: "raw" } : undefined;
}

// formatLimit renders a codemode bound by its wire name's unit.
function formatLimit(field: string, v: number): string {
  if (field.endsWith("_ns")) return formatMs(v / 1e6);
  if (field.endsWith("_bytes")) return formatBytes(v);
  return String(v);
}

// formatEnforced renders a value the child reported it enforced.
function formatEnforced(field: string, v: number | boolean): string {
  if (typeof v === "boolean") return v ? "yes" : "no";
  if (field.endsWith("_bytes")) return formatBytes(v);
  if (field === "cpu_seconds") return `${v}s`;
  return String(v);
}

// CodemodeReportView is what a program's completion says about the run it
// had: how it ended, how many calls it spent of its budget, the bounds the
// host asked for after the clamp (each one the clamp moved shown as
// declared → clamped), and what the child's kernel actually enforced, which
// a deployment's stricter limit or a whole-second CPU rounding can tighten.
function CodemodeReportView({ report, source }: { report: CodemodeReport; source: string }) {
  const adjusted = new Map((report.adjusted ?? []).map((a) => [a.field, a]));
  const callLimit = report.limits["tool_calls"];
  const fields = Object.keys(report.limits).sort();
  const enforced = report.confinement ? Object.entries(report.confinement).sort(([a], [b]) => a.localeCompare(b)) : [];
  return (
    <div className="mb-1.5 text-caption">
      {report.refusal && (
        <div className="mb-0.5">
          <span className="font-medium text-danger-fg">refused: </span>
          <span className="font-mono text-danger-fg">{report.refusal}</span>
        </div>
      )}
      <div className="mb-0.5 text-fg-muted">
        calls {report.calls}
        {callLimit !== undefined && ` / ${callLimit}`}
        {report.budget_refused ? ` · ${report.budget_refused} refused on budget` : ""}
        {report.state_calls ? ` · ${report.state_calls} run-state` : ""}
        {` · ${formatBytes(report.source_bytes)} of source`}
      </div>
      {report.effective_wall_ns !== undefined && (
        <div className="mb-0.5 text-warning-fg">
          wall cut to {formatMs(report.effective_wall_ns / 1e6)} by the node&apos;s deadline
        </div>
      )}
      <details className="mb-1">
        <summary className="cursor-pointer text-fg-muted px-1 py-0.5 rounded hover:bg-surface-2">
          limits requested{adjusted.size > 0 && ` (${adjusted.size} adjusted)`}
        </summary>
        <Table caption={`limits requested for ${source}`} density="sm" className="mt-1">
          <THead>
            <Th>bound</Th>
            <Th>requested, after the clamp</Th>
          </THead>
          <TBody>
            {fields.map((field) => {
              const a = adjusted.get(field);
              return (
                <Tr key={field} hover={false}>
                  <Th scope="row" className="font-mono font-normal text-fg-default">
                    {field}
                  </Th>
                  <Td className="font-mono text-fg-default">
                    {a ? (
                      <>
                        {formatLimit(field, a.requested)} → {formatLimit(field, a.applied)}{" "}
                        <span className="text-warning-fg">(adjusted)</span>
                      </>
                    ) : (
                      formatLimit(field, report.limits[field] ?? 0)
                    )}
                  </Td>
                </Tr>
              );
            })}
          </TBody>
        </Table>
      </details>
      {enforced.length > 0 && (
        <details className="mb-1">
          <summary className="cursor-pointer text-fg-muted px-1 py-0.5 rounded hover:bg-surface-2">
            enforced by the child
          </summary>
          <Table caption={`limits enforced for ${source}`} density="sm" className="mt-1">
            <THead>
              <Th>bound</Th>
              <Th>enforced</Th>
            </THead>
            <TBody>
              {enforced.map(([field, v]) => (
                <Tr key={field} hover={false}>
                  <Th scope="row" className="font-mono font-normal text-fg-default">
                    {field}
                  </Th>
                  <Td className="font-mono text-fg-default">{formatEnforced(field, v)}</Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        </details>
      )}
    </div>
  );
}

// NestedCalls lists the calls a program made inside its card — collapsible
// per program, the way the trace collapses a step — with each
// `call.parallel` batch kept together in its batch order.
function NestedCalls({ calls, source, runId }: { calls: ToolCall[]; source: string; runId: string }) {
  const [open, setOpen] = useState(true);
  const items = useMemo(() => nestedItems(calls), [calls]);
  const label = `nested calls (${calls.length})`;
  return (
    <div className="mb-1 rounded border border-border-default">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-label={`${label} of ${source}`}
        className="w-full flex items-center gap-2 px-2 py-1 text-left hover:bg-surface-2 rounded-t"
      >
        <span className="text-caption text-fg-muted">{label}</span>
        <span aria-hidden className="ml-auto text-fg-subtle text-caption">
          {open ? "▾" : "▸"}
        </span>
      </button>
      {open && (
        <ul className="border-t border-border-default p-2 space-y-2">
          {items.map((item) =>
            item.kind === "call" ? (
              <ToolCallCard key={item.call.seq} call={item.call} runId={runId} />
            ) : (
              <li key={`batch-${item.first}`} className="rounded border border-dashed border-border-default p-1.5">
                <div className="mb-1 text-caption font-mono text-fg-subtle">
                  call.parallel × {item.size}
                </div>
                <ul className="space-y-2">
                  {item.calls.map((c) => (
                    <ToolCallCard key={c.seq} call={c} runId={runId} />
                  ))}
                </ul>
              </li>
            ),
          )}
        </ul>
      )}
    </div>
  );
}

function ToolCallCard({ call, runId }: { call: ToolCall; runId: string }) {
  const [showRaw, setShowRaw] = useState(false);
  // What the card's repeated controls name themselves after: the tool and
  // the sequence number the card already shows, so two Bash calls in a row
  // are two distinct buttons to a screen reader.
  const source = `${call.toolName} seq ${call.seq}`;
  const program = call.toolName === CODEMODE_TOOL ? programOf(call) : undefined;
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
        <span className="font-medium text-micro">{call.toolName}</span>
        <StatusBadge
          status={call.running ? "running" : call.unpaired ? "none" : call.isError ? "failed" : "finished"}
          label={call.running ? "running" : call.unpaired ? "start, unpaired" : call.isError ? "error" : "ok"}
          showGlyph={false}
        />
        {call.emittedBy && (
          <span className="text-caption text-fg-muted">reported by the {call.emittedBy}</span>
        )}
        {call.duration !== undefined && (
          <span className="text-caption text-fg-muted">
            {formatMs(call.duration)}
          </span>
        )}
        <span className="ml-auto text-caption font-mono text-fg-muted">
          seq {call.seq}
        </span>
      </div>
      {summary.fields.length > 0 && <ToolFieldList fields={summary.fields} />}
      {summary.todos && summary.todos.length > 0 && (
        <TodoChecklist todos={summary.todos} />
      )}
      {call.errorMsg && (
        <div className="mb-1 rounded bg-danger-soft/40 px-1.5 py-1">
          <div className="flex items-center justify-between gap-2 mb-0.5">
            <span className="text-caption font-medium text-danger-fg">tool error</span>
            <CopyButton
              value={call.errorMsg}
              describes={`the error of ${source}`}
            />
          </div>
          <div className="text-caption font-mono text-danger-fg whitespace-pre-wrap break-words">
            {call.errorMsg}
          </div>
        </div>
      )}
      {call.codemode && <CodemodeReportView report={call.codemode} source={source} />}
      {program?.kind === "source" && (
        <ToolPayloadBlock label="program" source={source} value={program.text} runId={runId} kind="input" />
      )}
      {program?.kind === "omitted" && (
        <div className="mb-1 px-1 text-caption text-fg-muted">
          program not carried across the sandbox relay ({formatBytes(program.bytes)} in the container)
        </div>
      )}
      {program?.kind === "raw" && call.input && (
        <ToolPayloadBlock
          label="program"
          source={source}
          value={call.input}
          runId={runId}
          toolUseID={call.inputRef}
          kind="input"
          totalSize={call.inputSize}
        />
      )}
      {!program && call.input && (
        <ToolPayloadBlock
          label="raw input"
          source={source}
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
          source={source}
          value={call.output}
          runId={runId}
          toolUseID={call.outputRef}
          kind="output"
          totalSize={call.outputSize}
        />
      )}
      {call.nested && call.nested.length > 0 && (
        <NestedCalls calls={call.nested} source={source} runId={runId} />
      )}
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => setShowRaw((v) => !v)}
          aria-pressed={showRaw}
          aria-label={`${showRaw ? "hide raw" : "show raw"} event of ${source}`}
          className="text-caption text-fg-muted hover:text-fg-default"
        >
          {showRaw ? "hide raw" : "show raw"}
        </button>
        {showRaw && call.rawData && (
          <CopyButton
            value={JSON.stringify(call.rawData, null, 2)}
            describes={`the raw event of ${source}`}
          />
        )}
      </div>
      {showRaw && call.rawData && (
        <pre className="mt-1 text-caption font-mono whitespace-pre-wrap break-all bg-surface-1 rounded p-1.5 text-fg-muted">
          {JSON.stringify(call.rawData, null, 2)}
        </pre>
      )}
    </li>
  );
}
