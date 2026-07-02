import { memo, useCallback } from "react";

import { EVENT_BADGE, indentForType, type AnnotatedEvent } from "./eventModel";
import {
  ToolPayloadBlock,
  pickInlinePayload,
  pickRef,
  pickSize,
} from "../detail/ToolCalls";

// Event types that carry a tool I/O payload worth expanding inline (the
// timeline equivalent of the per-node Tools tab). tool_started carries the
// input; tool_called the output; tool_error may carry either plus an error.
const TOOL_PAYLOAD_TYPES = new Set(["tool_started", "tool_called", "tool_error"]);

// Memoised so Virtuoso can skip re-rendering the ~20 visible rows when
// only ancillary parent state changes (followTail toggle, filter chip
// hover, etc.). The annotated event cache upstream preserves identity
// for unchanged events, and `onSelectNodeIteration` is useCallback'd by
// RunView — so the memo actually hits on subsequent log chunks.
export const EventRow = memo(function EventRow({
  ann,
  onSelectNodeIteration,
}: {
  ann: AnnotatedEvent;
  onSelectNodeIteration?: (nodeId: string, index: number) => void;
}) {
  const e = ann.event;
  const badge = EVENT_BADGE[e.type] ?? "bg-surface-2 text-fg-muted";
  const indent = indentForType(e.type);
  const handleClick = useCallback(() => {
    if (e.node_id && onSelectNodeIteration && ann.executionIndex >= 0) {
      onSelectNodeIteration(e.node_id, ann.executionIndex);
    }
  }, [e.node_id, ann.executionIndex, onSelectNodeIteration]);

  // For tool events, surface the full input/output inline as expandable
  // <details> (reusing the Tools-tab payload block: preview + expand +
  // load-more + copy) so a user watching the run doesn't have to open the
  // per-node detail panel to read a truncated command or a long result —
  // the timeline reads like Claude Code's tool cards.
  const data = e.data ?? {};
  const isToolPayload = TOOL_PAYLOAD_TYPES.has(e.type);
  const input = isToolPayload ? pickInlinePayload(data, "input") : undefined;
  const output = isToolPayload ? pickInlinePayload(data, "output") : undefined;
  const errorMsg =
    e.type === "tool_error"
      ? ((data["error"] as string) ?? (data["message"] as string) ?? undefined)
      : undefined;

  return (
    <div className="w-full">
      <button
        type="button"
        onClick={handleClick}
        className="w-full grid grid-cols-[auto_auto_auto_1fr] gap-2 py-0.5 text-left font-mono text-caption hover:bg-surface-2 rounded px-1"
        title={
          e.node_id
            ? `Jump to ${e.node_id} (attempt ${ann.executionIndex + 1})`
            : undefined
        }
      >
        <span className="text-fg-subtle">{e.seq.toString().padStart(4, "0")}</span>
        <span
          className={`px-1.5 rounded ${badge}`}
          style={indent > 0 ? { marginLeft: indent * 12 } : undefined}
        >
          {indent > 0 && (
            <span className="text-fg-subtle mr-1" aria-hidden="true">
              ↳
            </span>
          )}
          {e.type}
        </span>
        <span className="text-fg-default truncate">{e.node_id ?? "-"}</span>
        <span className="text-fg-subtle truncate">{ann.preview}</span>
      </button>

      {(input || output || errorMsg) && (
        <div className="pl-8 pr-1 pb-0.5">
          {input && (
            <ToolPayloadBlock
              label="input"
              value={input}
              runId={e.run_id}
              toolUseID={pickRef(data, "input")}
              kind="input"
              totalSize={pickSize(data, "input")}
            />
          )}
          {output && (
            <ToolPayloadBlock
              label="output"
              value={output}
              runId={e.run_id}
              toolUseID={pickRef(data, "output")}
              kind="output"
              totalSize={pickSize(data, "output")}
            />
          )}
          {errorMsg && (
            <div className="mb-1 rounded bg-danger-soft/40 px-1.5 py-1 text-caption font-mono text-danger-fg whitespace-pre-wrap break-words">
              {errorMsg}
            </div>
          )}
        </div>
      )}
    </div>
  );
});
