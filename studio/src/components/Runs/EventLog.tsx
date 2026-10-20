import { useMemo } from "react";

import { Virtuoso } from "react-virtuoso";
import { ChevronDownIcon } from "@radix-ui/react-icons";

import type { RunEvent } from "@/api/runs";
import { Checkbox, IconButton, Input } from "@/components/ui";

import { EVENT_BADGE, type AnnotatedEvent } from "./eventLog/eventModel";
import { EventRow } from "./eventLog/EventRow";
import { useAnnotatedEvents } from "./eventLog/useAnnotatedEvents";
import { usePersistedEventFilters } from "./eventLog/usePersistedEventFilters";
import {
  AT_BOTTOM_THRESHOLD_PX,
  useVirtuosoTailFollow,
} from "./eventLog/useVirtuosoTailFollow";

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
  const {
    search,
    setSearch,
    activeTypes,
    toggleActiveType,
  } = usePersistedEventFilters(runId);

  const { annotated, typeCounts } = useAnnotatedEvents(events);

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

  const {
    virtuosoRef,
    handleToggleFollow,
    jumpToNextError,
    handleIsScrolling,
    handleAtBottomStateChange,
  } = useVirtuosoTailFollow({
    followTail,
    filteredLength: filtered.length,
    errorIndices,
    onToggleFollow,
  });

  return (
    <div className="h-full flex flex-col bg-surface-1 min-h-0">
      <div className="px-3 py-1.5 border-b border-border-default flex flex-wrap items-center gap-2 text-micro">
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
            className="text-caption text-fg-subtle hover:text-fg-default underline"
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
            aria-label="Search events"
            size="sm"
            leadingIcon={<span className="text-caption">⌕</span>}
          />
        </div>
        <label className="ml-auto inline-flex items-center gap-1.5 cursor-pointer">
          <Checkbox
            checked={followTail}
            onChange={(e) => handleToggleFollow(e.target.checked)}
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
                onClick={() => toggleActiveType(t)}
                className={`text-caption px-1.5 py-0.5 rounded border transition-colors ${
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
          <div className="text-fg-subtle py-2 text-micro">
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
            isScrolling={handleIsScrolling}
            atBottomStateChange={handleAtBottomStateChange}
            itemContent={(_, ann) => (
              <EventRow ann={ann} onSelectNodeIteration={onSelectNodeIteration} />
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
