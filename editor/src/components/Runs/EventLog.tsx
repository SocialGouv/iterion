import { useMemo, useRef, useState } from "react";
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
  // node + iteration in the canvas + detail panel.
  onSelectNodeIteration?: (nodeId: string, iteration: number) => void;
  // Clear the execution filter (typically a "Clear" affordance shown
  // in the toolbar when an execution is selected).
  onClearSelection?: () => void;
  onCollapse?: () => void;
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
  // Iteration as derived from node_started ordering. Used to drive
  // cross-highlighting when the user clicks the row.
  iteration: number;
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
}: Props) {
  const [search, setSearch] = useState("");
  const [activeTypes, setActiveTypes] = useState<Set<string>>(() => new Set());
  const virtuosoRef = useRef<VirtuosoHandle>(null);
  // Tracks whether virtuoso is currently scrolling. atBottomStateChange
  // also fires on data/filter changes; we only treat "left the bottom"
  // as a user intent to disable follow-tail when an actual scroll is in
  // flight.
  const isScrollingRef = useRef<boolean>(false);

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
  } | null>(null);

  const { annotated, typeCounts } = useMemo(() => {
    const cache = cacheRef.current;
    let baseAnnotated: AnnotatedEvent[];
    let counts: Map<string, number>;
    let typeCountsMap: Map<string, number>;
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
      baseAnnotated = cache.annotated;
      counts = new Map(cache.counts);
      typeCountsMap = new Map(cache.typeCounts);
      startIdx = cachedLen;
    } else {
      baseAnnotated = [];
      counts = new Map<string, number>();
      typeCountsMap = new Map<string, number>();
    }

    for (let i = startIdx; i < events.length; i++) {
      const e = events[i]!;
      baseAnnotated.push({
        event: e,
        iteration: stepIteration(counts, e),
        preview: previewData(e.data),
      });
      typeCountsMap.set(e.type, (typeCountsMap.get(e.type) ?? 0) + 1);
    }

    cacheRef.current = {
      events,
      annotated: baseAnnotated,
      counts,
      typeCounts: typeCountsMap,
    };
    return { annotated: baseAnnotated, typeCounts: typeCountsMap };
  }, [events]);

  const knownTypes = useMemo(
    () => Array.from(typeCounts.keys()).sort(),
    [typeCounts],
  );

  const filtered = useMemo(() => {
    const query = search.trim().toLowerCase();
    return annotated.filter(({ event: e, iteration }) => {
      // Execution selection filter (cross-highlight from the canvas).
      if (selectedExecutionId) {
        if (!e.node_id) return false;
        const branch = e.branch_id || "main";
        const id = `exec:${branch}:${e.node_id}:${iteration}`;
        if (id !== selectedExecutionId) return false;
      }
      if (activeTypes.size > 0 && !activeTypes.has(e.type)) return false;
      if (!query) return true;
      if (e.type.toLowerCase().includes(query)) return true;
      if (e.node_id?.toLowerCase().includes(query)) return true;
      if (e.data && JSON.stringify(e.data).toLowerCase().includes(query))
        return true;
      return false;
    });
    // `annotated` is mutated in place when the cache extends — depend
    // on `events` so this memo invalidates on every batch flush even
    // when the array reference is unchanged.
  }, [annotated, events, selectedExecutionId, activeTypes, search]);

  const handleToggleFollow = (next: boolean) => {
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
            followOutput={followTail ? "auto" : false}
            atBottomThreshold={AT_BOTTOM_THRESHOLD_PX}
            isScrolling={(s) => {
              isScrollingRef.current = s;
            }}
            atBottomStateChange={(atBottom) => {
              if (!atBottom && followTail && isScrollingRef.current) {
                onToggleFollow(false);
              }
            }}
            itemContent={(_, ann) => (
              <EventRow
                ann={ann}
                onSelect={() => {
                  if (ann.event.node_id && onSelectNodeIteration) {
                    onSelectNodeIteration(ann.event.node_id, ann.iteration);
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

function EventRow({
  ann,
  onSelect,
}: {
  ann: AnnotatedEvent;
  onSelect: () => void;
}) {
  const e = ann.event;
  const badge = EVENT_BADGE[e.type] ?? "bg-surface-2 text-fg-muted";
  return (
    <button
      type="button"
      onClick={onSelect}
      className="w-full grid grid-cols-[auto_auto_auto_1fr] gap-2 py-0.5 text-left font-mono text-[10px] hover:bg-surface-2 rounded px-1"
      title={
        e.node_id
          ? `Jump to ${e.node_id} (iteration ${ann.iteration + 1})`
          : undefined
      }
    >
      <span className="text-fg-subtle">{e.seq.toString().padStart(4, "0")}</span>
      <span className={`px-1.5 rounded ${badge}`}>{e.type}</span>
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
