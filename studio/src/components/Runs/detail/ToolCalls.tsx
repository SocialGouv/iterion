import {
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import type { RunEvent } from "@/api/runs";
import { fetchToolBlob } from "@/api/runs";
import { formatBytes, formatMs } from "@/lib/format";
import { errorMessage } from "@/lib/errorHints";
import { CopyButton, StatusBadge } from "@/components/ui";

import { formatToolCall, type ToolField, type TodoItem } from "../toolFormatters";

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
  // addressable via fetchToolBlob(runId, ref, kind). The studio renders
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
export function pickInlinePayload(
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
// The presence of a ref tells the studio "there's more to fetch beyond
// the preview" — render the Load more affordance.
export function pickRef(
  data: Record<string, unknown>,
  kind: "input" | "output",
): string | undefined {
  const ref = data[`${kind}_ref`];
  return typeof ref === "string" && ref !== "" ? ref : undefined;
}

// pickSize returns the total byte size of the original tool payload as
// declared on the event (`{kind}_size`). Used to size Load more progress
// affordances. Returns undefined when the event predates this plumbing.
export function pickSize(
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
export function useToolCalls(matching: RunEvent[]): ToolCall[] {
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
      <summary className="cursor-pointer text-caption text-fg-muted px-1 py-0.5 rounded hover:bg-surface-2 flex items-center justify-between">
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
            className="text-caption text-fg-subtle hover:text-fg-default px-1 py-0.5 rounded hover:bg-surface-2 disabled:opacity-50"
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
        <span className="font-medium text-micro">{call.toolName}</span>
        <StatusBadge
          status={call.isError ? "failed" : "finished"}
          label={call.isError ? "error" : "ok"}
          showGlyph={false}
        />
        {call.duration !== undefined && (
          <span className="text-caption text-fg-subtle">
            {formatMs(call.duration)}
          </span>
        )}
        <span className="ml-auto text-caption font-mono text-fg-subtle">
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
            <CopyButton value={call.errorMsg} />
          </div>
          <div className="text-caption font-mono text-danger-fg whitespace-pre-wrap break-words">
            {call.errorMsg}
          </div>
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
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => setShowRaw((v) => !v)}
          aria-pressed={showRaw}
          className="text-caption text-fg-subtle hover:text-fg-default"
        >
          {showRaw ? "hide raw" : "show raw"}
        </button>
        {showRaw && call.rawData && (
          <CopyButton value={JSON.stringify(call.rawData, null, 2)} />
        )}
      </div>
      {showRaw && call.rawData && (
        <pre className="mt-1 text-caption font-mono whitespace-pre-wrap break-all bg-surface-1 rounded p-1.5 text-fg-subtle">
          {JSON.stringify(call.rawData, null, 2)}
        </pre>
      )}
    </li>
  );
}

function safeJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
