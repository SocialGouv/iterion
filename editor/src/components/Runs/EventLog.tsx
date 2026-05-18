import { useEffect, useMemo, useRef, useState } from "react";
import { Virtuoso, type VirtuosoHandle } from "react-virtuoso";
import { ChevronDownIcon } from "@radix-ui/react-icons";

import type { RunEvent } from "@/api/runs";
import { IconButton, Input } from "@/components/ui";
import { stepIteration } from "@/lib/eventIter";

interface Props {
  events: RunEvent[];
  // When non-null, the log filters to events that match the iteration
  // for this exec (mirrors the previous behaviour). The cross-hairs
  // selection from the canvas drives this.
  selectedExecutionId: string | null;
  followTail: boolean;
  onToggleFollow: (next: boolean) => void;
  // Wired by RunView so a click on an event jumps to the matching
  // node + attempt in the canvas + detail panel. The second arg is a
  // 0-based ARRAY INDEX into the node's executions list (not a scalar
  // loop_iteration — those aren't unique under Option 3 nested loops).
  onSelectNodeIteration?: (nodeId: string, index: number) => void;
  // Clear the execution filter (typically a "Clear" affordance shown
  // in the toolbar when an execution is selected).
  onClearSelection?: () => void;
  onCollapse?: () => void;
  // When provided, the search query and type filter chips are persisted
  // to localStorage under a per-run key so coming back to the same run
  // restores the previous filter state.
  runId?: string | null;
}

// Per-run filter persistence: `run-console.event-filters.v1.<runId>`.
// Schema is intentionally minimal so older entries can stay readable
// when fields grow; missing keys fall back to defaults.
interface PersistedFilters {
  search?: string;
  types?: string[];
}

function filterStorageKey(runId: string): string {
  return `run-console.event-filters.v1.${runId}`;
}

function loadPersistedFilters(runId: string): PersistedFilters | null {
  try {
    const raw = window.localStorage.getItem(filterStorageKey(runId));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as PersistedFilters;
    return parsed && typeof parsed === "object" ? parsed : null;
  } catch {
    return null;
  }
}

function savePersistedFilters(runId: string, value: PersistedFilters) {
  try {
    // Treat all-default as "delete entry" to keep storage clean. The
    // common case (no filter applied) writes nothing.
    if ((!value.search || value.search === "") && (!value.types || value.types.length === 0)) {
      window.localStorage.removeItem(filterStorageKey(runId));
      return;
    }
    window.localStorage.setItem(filterStorageKey(runId), JSON.stringify(value));
  } catch {
    // quota / privacy mode → silently degrade to non-persistent
  }
}

// Slack the bottom-detection threshold so dynamic-height row reflows
// don't transiently report "not at bottom" while followOutput re-aligns.
const AT_BOTTOM_THRESHOLD_PX = 48;

const EVENT_BADGE: Record<string, string> = {
  run_started: "bg-info-soft text-info-fg",
  run_finished: "bg-success-soft text-success-fg",
  run_failed: "bg-danger-soft text-danger-fg",
  run_paused: "bg-warning-soft text-warning-fg",
  run_resumed: "bg-info-soft text-info-fg",
  run_cancelled: "bg-surface-2 text-fg-muted",
  node_started: "bg-info-soft text-info-fg",
  node_finished: "bg-success-soft text-success-fg",
  artifact_written: "bg-accent-soft text-fg-default",
  human_input_requested: "bg-warning-soft text-warning-fg",
  budget_warning: "bg-warning-soft text-warning-fg",
  budget_exceeded: "bg-danger-soft text-danger-fg",
  llm_request: "bg-surface-2 text-fg-muted",
  llm_step_finished: "bg-surface-2 text-fg-muted",
  tool_called: "bg-surface-2 text-fg-muted",
  tool_error: "bg-danger-soft text-danger-fg",
};

interface AnnotatedEvent {
  event: RunEvent;
  // Scalar `iteration` from the event's data (for display in the row
  // header). NOT unique post-Option-3 — the runtime's
  // currentLoopIteration can return the same max() across multiple
  // attempts when an outer loop counter dominates the inner.
  iteration: number;
  // 0-based count of node_started events for this (branch, node) up to
  // and including this row. Used as the array index in the per-node
  // executions list when the user clicks the row to jump to its exec.
  executionIndex: number;
  // execution_id of the exec this event belongs to (the most recent
  // node_started for this branch+node, or this event's own id if it
  // IS a node_started). Used by the selection filter when cross-
  // highlighting from the canvas.
  executionId: string | null;
  // Pre-computed match against the current filters; cached so the
  // virtual list doesn't recompute per scroll frame.
  preview: string;
}

export default function EventLog({
  events,
  selectedExecutionId,
  followTail,
  onToggleFollow,
  onSelectNodeIteration,
  onClearSelection,
  onCollapse,
  runId,
}: Props) {
  // Lazy initial value: read localStorage once on mount so the chips
  // and search box render with the persisted state from the get-go.
  const initialPersisted = useMemo<PersistedFilters | null>(
    () => (runId ? loadPersistedFilters(runId) : null),
    // The runId is treated as stable for this component's lifetime
    // (RunView remounts EventLog when navigating between runs), so this
    // memo intentionally runs once.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );
  const [search, setSearch] = useState(initialPersisted?.search ?? "");
  const [activeTypes, setActiveTypes] = useState<Set<string>>(
    () => new Set(initialPersisted?.types ?? []),
  );

  // Persist on every change. We avoid debouncing the search input
  // because the writes are small and infrequent compared to typing
  // bursts in other inputs, and an immediate write means a hard reload
  // never loses keystrokes.
  useEffect(() => {
    if (!runId) return;
    savePersistedFilters(runId, {
      search: search || undefined,
      types: activeTypes.size > 0 ? Array.from(activeTypes) : undefined,
    });
  }, [runId, search, activeTypes]);
  const virtuosoRef = useRef<VirtuosoHandle>(null);
  // Tracks whether virtuoso is currently scrolling. atBottomStateChange
  // also fires on data/filter changes; we only treat "left the bottom"
  // as a user intent to disable follow-tail when an actual scroll is in
  // flight.
  const isScrollingRef = useRef<boolean>(false);
  // True when the most recent follow-tail disable came from a scroll-up
  // (vs the checkbox). Lets us auto-re-engage when the user scrolls back
  // to the tail, while keeping a manual uncheck sticky.
  const disabledByScrollRef = useRef<boolean>(false);

  // Incremental cache: when `events` only grows by appending at the
  // tail (the common live case once history is replayed), reuse the
  // previously-annotated prefix and only annotate the new tail. Falls
  // back to a full recompute when the array prefix changes (snapshot
  // replay, MAX_EVENTS eviction, runId switch). This drops per-event
  // cost from O(N) to O(K) where K is the number of new events.
  const cacheRef = useRef<{
    events: RunEvent[];
    annotated: AnnotatedEvent[];
    counts: Map<string, number>;
    typeCounts: Map<string, number>;
    // Per-(branch, node) counters and last-seen exec_id, paralleling
    // the live store's lastExecIDByNode. Threading these across cache
    // invocations is what makes the annotation pass O(K) on the new
    // tail rather than O(N) on every batch flush.
    execIndexCounts: Map<string, number>;
    lastExecID: Map<string, string>;
  } | null>(null);

  const { annotated, typeCounts } = useMemo(() => {
    const cache = cacheRef.current;
    let baseAnnotated: AnnotatedEvent[];
    let counts: Map<string, number>;
    let typeCountsMap: Map<string, number>;
    let execIndexCounts: Map<string, number>;
    let lastExecID: Map<string, string>;
    let startIdx = 0;

    // Reuse the cache when the live events array has only grown at the
    // tail. A snapshot replay produces a fresh `events` array whose
    // RunEvent objects are deserialized from scratch — the reference
    // check at index 0 detects that boundary and forces a full
    // recompute. The cached array is mutated in place below; nothing
    // outside this useMemo retains it across renders.
    const cachedLen = cache?.annotated.length ?? 0;
    const reusable =
      cache !== null &&
      cachedLen > 0 &&
      cachedLen <= events.length &&
      cache.annotated[0]!.event === events[0] &&
      cache.annotated[cachedLen - 1]!.event === events[cachedLen - 1];

    if (reusable) {
      // Start a fresh array so consumers (useMemo dep, downstream
      // filter passes) see a new reference when events grow — the
      // previous in-place push kept the same array identity and
      // relied on the events dep change as a fence, which is fragile
      // if any caller ever mutated the source events array in place.
      baseAnnotated = cache.annotated.slice();
      counts = new Map(cache.counts);
      typeCountsMap = new Map(cache.typeCounts);
      execIndexCounts = new Map(cache.execIndexCounts);
      lastExecID = new Map(cache.lastExecID);
      startIdx = cachedLen;
    } else {
      baseAnnotated = [];
      counts = new Map<string, number>();
      typeCountsMap = new Map<string, number>();
      execIndexCounts = new Map<string, number>();
      lastExecID = new Map<string, string>();
    }

    for (let i = startIdx; i < events.length; i++) {
      const e = events[i]!;
      const iteration = stepIteration(counts, e);
      // Track exec_id per (branch, node) so non-node_started events
      // attribute to the right exec for the selection filter, and so
      // node_started's own count gives a stable 0-based array index.
      const branch = e.branch_id || "main";
      const key = e.node_id ? `${branch}\t${e.node_id}` : "";
      let executionId: string | null = null;
      let executionIndex = -1;
      if (e.node_id) {
        if (e.type === "node_started") {
          // New exec_id, prefer iteration_path when present (mirror of
          // the live store reducer and pkg/runview/snapshot.go).
          const rawPath = e.data?.iteration_path;
          executionId =
            typeof rawPath === "string" && rawPath.length > 0
              ? `exec:${branch}:${e.node_id}:${rawPath}`
              : `exec:${branch}:${e.node_id}:${iteration}`;
          lastExecID.set(key, executionId);
          const prevIdx = execIndexCounts.get(key);
          executionIndex = prevIdx === undefined ? 0 : prevIdx + 1;
          execIndexCounts.set(key, executionIndex);
        } else {
          executionId = lastExecID.get(key) ?? null;
          executionIndex = execIndexCounts.get(key) ?? 0;
        }
      }
      baseAnnotated.push({
        event: e,
        iteration,
        executionIndex,
        executionId,
        preview: previewData(e.data),
      });
      typeCountsMap.set(e.type, (typeCountsMap.get(e.type) ?? 0) + 1);
    }

    cacheRef.current = {
      events,
      annotated: baseAnnotated,
      counts,
      typeCounts: typeCountsMap,
      execIndexCounts,
      lastExecID,
    };
    return { annotated: baseAnnotated, typeCounts: typeCountsMap };
  }, [events]);

  const knownTypes = useMemo(
    () => Array.from(typeCounts.keys()).sort(),
    [typeCounts],
  );

  // Event types that the run console should surface as "errors" — the
  // user wants these immediately visible without scrolling. budget_*
  // sits next to the hard failures because exceeding budget is a
  // workflow-level abort, not a soft warning.
  const errorEventTypes = useMemo(
    () => new Set<string>(["run_failed", "tool_error", "budget_exceeded"]),
    [],
  );
  const errorCount = useMemo(() => {
    let n = 0;
    for (const t of errorEventTypes) n += typeCounts.get(t) ?? 0;
    return n;
  }, [typeCounts, errorEventTypes]);

  // Compute the filtered list and the indices of error events in one
  // pass. Walking `filtered` again on every "next error" click would
  // be O(N) on a 100k-event log; deriving alongside the filter keeps
  // the click handler O(1).
  const { filtered, errorIndices } = useMemo(() => {
    const query = search.trim().toLowerCase();
    const out: AnnotatedEvent[] = [];
    const errIdx: number[] = [];
    for (const ann of annotated) {
      const e = ann.event;
      if (selectedExecutionId && ann.executionId !== selectedExecutionId) continue;
      if (activeTypes.size > 0 && !activeTypes.has(e.type)) continue;
      if (query) {
        let matches = false;
        if (e.type.toLowerCase().includes(query)) matches = true;
        else if (e.node_id?.toLowerCase().includes(query)) matches = true;
        else if (e.data && JSON.stringify(e.data).toLowerCase().includes(query))
          matches = true;
        if (!matches) continue;
      }
      const idx = out.length;
      out.push(ann);
      if (errorEventTypes.has(e.type)) errIdx.push(idx);
    }
    return { filtered: out, errorIndices: errIdx };
    // `annotated` is mutated in place when the cache extends — depend
    // on `events` so this memo invalidates on every batch flush even
    // when the array reference is unchanged.
  }, [annotated, events, selectedExecutionId, activeTypes, search, errorEventTypes]);

  // Cycle through error events on repeated clicks of the "n errors"
  // badge: scroll to the first one, then the next, etc. — wraps around
  // at the end. The cursor sits in a ref so the parent doesn't
  // re-render between clicks.
  const errorCursorRef = useRef<number>(-1);
  const jumpToNextError = () => {
    if (errorIndices.length === 0) return;
    errorCursorRef.current = (errorCursorRef.current + 1) % errorIndices.length;
    const target = errorIndices[errorCursorRef.current]!;
    virtuosoRef.current?.scrollToIndex({
      index: target,
      align: "center",
      behavior: "smooth",
    });
    // Disengage tail-follow so the auto-scroll doesn't immediately
    // yank the user back to live.
    if (followTail) onToggleFollow(false);
  };

  // Virtuoso's `followOutput="auto"` only fires when it considers the
  // user "at bottom", which is unreliable on a live run where events
  // arrive across multiple micro-tasks: between batches, dynamic-height
  // measurement can briefly report not-at-bottom and skip the scroll.
  // Drive the scroll explicitly from `filtered.length` while followTail
  // is on; the `atBottomStateChange` disengage below still flips the
  // toggle off when the user scrolls up.
  useEffect(() => {
    if (followTail && filtered.length > 0) {
      virtuosoRef.current?.scrollToIndex({
        index: filtered.length - 1,
        align: "end",
        behavior: "auto",
      });
    }
  }, [followTail, filtered.length]);

  const handleToggleFollow = (next: boolean) => {
    // A direct checkbox interaction is always treated as manual intent,
    // overriding any prior "disabled by scroll" memory.
    disabledByScrollRef.current = false;
    onToggleFollow(next);
    // Re-engaging the toggle while scrolled up shouldn't wait for the
    // next event to arrive — jump to the tail immediately.
    if (next && filtered.length > 0) {
      virtuosoRef.current?.scrollToIndex({
        index: filtered.length - 1,
        align: "end",
        behavior: "auto",
      });
    }
  };

  const toggleType = (t: string) => {
    setActiveTypes((prev) => {
      const next = new Set(prev);
      if (next.has(t)) next.delete(t);
      else next.add(t);
      return next;
    });
  };

  return (
    <div className="h-full flex flex-col bg-surface-1 min-h-0">
      <div className="px-3 py-1.5 border-b border-border-default flex flex-wrap items-center gap-2 text-[11px]">
        <span className="font-medium text-fg-muted">Events</span>
        <span className="text-fg-subtle">
          {filtered.length} / {events.length}
        </span>
        {errorCount > 0 && (
          <button
            type="button"
            onClick={jumpToNextError}
            className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-danger-soft text-danger-fg border border-danger/40 hover:bg-danger-soft/80 transition-colors"
            title={`${errorCount} error event${errorCount === 1 ? "" : "s"} — click to jump to the next`}
          >
            <span aria-hidden="true">●</span>
            <span>
              {errorCount} error{errorCount === 1 ? "" : "s"}
            </span>
          </button>
        )}
        {selectedExecutionId && (
          <button
            type="button"
            onClick={() => onClearSelection?.()}
            className="text-[10px] text-fg-subtle hover:text-fg-default underline"
            title="Clear execution filter"
          >
            clear filter
          </button>
        )}
        <div className="flex-1 min-w-[140px] max-w-[320px]">
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search events…"
            size="sm"
            leadingIcon={<span className="text-[10px]">⌕</span>}
          />
        </div>
        <label className="ml-auto inline-flex items-center gap-1.5 cursor-pointer">
          <input
            type="checkbox"
            checked={followTail}
            onChange={(e) => handleToggleFollow(e.target.checked)}
            className="accent-accent"
          />
          Follow tail
        </label>
        {onCollapse && (
          <IconButton
            label="Hide event log"
            size="sm"
            variant="ghost"
            onClick={onCollapse}
          >
            <ChevronDownIcon />
          </IconButton>
        )}
      </div>
      {knownTypes.length > 1 && (
        <div className="px-3 py-1 border-b border-border-default flex flex-wrap gap-1">
          {knownTypes.map((t) => {
            const isActive = activeTypes.has(t);
            const badge = EVENT_BADGE[t] ?? "bg-surface-2 text-fg-muted";
            return (
              <button
                key={t}
                type="button"
                onClick={() => toggleType(t)}
                className={`text-[10px] px-1.5 py-0.5 rounded border transition-colors ${
                  isActive
                    ? `${badge} border-accent`
                    : "bg-surface-1 border-border-default text-fg-subtle hover:text-fg-default"
                }`}
              >
                {t}{" "}
                <span className="text-fg-subtle font-mono">
                  {typeCounts.get(t)}
                </span>
              </button>
            );
          })}
        </div>
      )}
      <div className="flex-1 min-h-0 px-3 py-1">
        {filtered.length === 0 ? (
          <div className="text-fg-subtle py-2 text-[11px]">
            {events.length === 0 ? "No events yet." : "No events match."}
          </div>
        ) : (
          <Virtuoso
            ref={virtuosoRef}
            className="h-full"
            data={filtered}
            initialTopMostItemIndex={
              followTail
                ? { index: filtered.length - 1, align: "end" }
                : 0
            }
            followOutput={followTail ? "auto" : false}
            atBottomThreshold={AT_BOTTOM_THRESHOLD_PX}
            isScrolling={(s) => {
              isScrollingRef.current = s;
            }}
            atBottomStateChange={(atBottom) => {
              if (!atBottom && followTail && isScrollingRef.current) {
                disabledByScrollRef.current = true;
                onToggleFollow(false);
              } else if (
                atBottom &&
                !followTail &&
                disabledByScrollRef.current
              ) {
                // No isScrolling guard: atBottomStateChange(true) can race
                // with isScrolling(false) on momentum scrolls, leaving the
                // ref already cleared. disabledByScrollRef alone is enough
                // to distinguish scroll-disabled from manually-disabled.
                disabledByScrollRef.current = false;
                onToggleFollow(true);
              }
            }}
            itemContent={(_, ann) => (
              <EventRow
                ann={ann}
                onSelect={() => {
                  if (ann.event.node_id && onSelectNodeIteration && ann.executionIndex >= 0) {
                    onSelectNodeIteration(ann.event.node_id, ann.executionIndex);
                  }
                }}
              />
            )}
            computeItemKey={(_, ann) =>
              `${ann.event.run_id}:${ann.event.seq}`
            }
          />
        )}
      </div>
    </div>
  );
}

// indentForType returns a visual nesting level for the event log. The
// goal is to make multi-turn LLM rounds and their tool calls visually
// "owned" by the surrounding llm_request, so the eye can scan
// turn-boundaries instead of treating every event as a sibling. The
// runtime doesn't carry an explicit parent_seq on retries / tool calls,
// so we lean on the event taxonomy itself.
function indentForType(t: string): number {
  switch (t) {
    case "llm_step_finished":
    case "llm_retry":
    case "tool_started":
    case "tool_called":
    case "tool_error":
    case "human_input_requested":
      return 1;
    case "artifact_written":
      return 1;
    default:
      return 0;
  }
}

function EventRow({
  ann,
  onSelect,
}: {
  ann: AnnotatedEvent;
  onSelect: () => void;
}) {
  const e = ann.event;
  const badge = EVENT_BADGE[e.type] ?? "bg-surface-2 text-fg-muted";
  const indent = indentForType(e.type);
  return (
    <button
      type="button"
      onClick={onSelect}
      className="w-full grid grid-cols-[auto_auto_auto_1fr] gap-2 py-0.5 text-left font-mono text-[10px] hover:bg-surface-2 rounded px-1"
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
  );
}

function previewData(data: Record<string, unknown> | undefined): string {
  if (!data) return "";
  const interesting = [
    "kind",
    "model",
    "tool",
    "tool_name",
    "version",
    "publish",
    "to",
    "loop",
    "iteration",
    "error",
    "input_tokens",
    "output_tokens",
  ];
  const parts: string[] = [];
  for (const k of interesting) {
    if (data[k] !== undefined) parts.push(`${k}=${formatValue(data[k])}`);
  }
  return parts.join(" ");
}

function formatValue(v: unknown): string {
  if (typeof v === "string") {
    return v.length > 60 ? v.slice(0, 57) + "…" : v;
  }
  return String(v);
}
