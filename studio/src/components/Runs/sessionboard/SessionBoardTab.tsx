import { Suspense, lazy, useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useShallow } from "zustand/react/shallow";

import { getSessionBoard } from "@/api/runs";
import { useServerInfoStore } from "@/store/serverInfo";
import { Badge, EmptyState, LiveDot } from "@/components/ui";
import { humanizeKey } from "@/lib/humanizeKey";
import {
  selectTodoTimeline,
  useRunStore,
  type TodoTimelineEntry,
} from "@/store/run";
import { TodoItems } from "@/components/Runs/todoChecklist";
import { STATUS_GLYPH, countByStatus } from "@/components/Runs/todoStatus";

// Lazy so Recharts (heavy) is only fetched when a run actually has curated
// widgets — most runs never opt into the curation layer.
const SessionWidgets = lazy(() => import("./widgets"));

// SessionBoardTab is the friendly, persistent per-run "Tasks" view. It is
// deliberately NOT a re-skin of the technical run view (events / logs /
// graph / cost): it shows only the agent's task list — the current node's
// live list ("Now") plus the history of earlier nodes' lists this run
// ("Earlier this run") — in plain language, surviving run completion.
//
// Data comes from `todoHistoryByExec` (never cleared) via
// selectTodoTimeline, so a finished run still renders. On mount we ensure
// the persisted /events log is folded in (loadEventHistoryIfMissing) so a
// cold-loaded run reconstructs its task history without a live stream.
export function SessionBoardTab({ runId }: { runId: string }) {
  const loadHistory = useRunStore((s) => s.loadEventHistoryIfMissing);
  const timeline = useRunStore(useShallow((s) => selectTodoTimeline(s)));
  // last_seq advances on every run event; we use it as the refetch trigger
  // for the curated widget spec (which the LLM coordinator updates out of
  // band, infrequently).
  const lastSeq = useRunStore((s) => s.snapshot?.last_seq ?? -1);
  const widgets = useSessionBoardWidgets(runId, lastSeq);

  useEffect(() => {
    if (!runId) return;
    void loadHistory(runId).catch(() => {});
  }, [runId, loadHistory]);

  if (timeline.length === 0 && widgets.length === 0) {
    return (
      <div className="h-full min-h-0">
        <EmptyState
          title="No task list yet"
          message="When the agent plans its work, its checklist appears here — and stays, so you can follow the whole session at a glance."
          caret
        />
      </div>
    );
  }

  const hasTasks = timeline.length > 0;
  const currentIdx = hasTasks ? pickCurrentIndex(timeline) : -1;
  // Earlier entries, most-recent first (closest to "Now" on top).
  const earlier = hasTasks
    ? timeline.filter((_, i) => i !== currentIdx).reverse()
    : [];

  return (
    <div className="h-full min-h-0 overflow-y-auto px-3 py-3 flex flex-col gap-4">
      {hasTasks && (
        <section className="flex flex-col gap-2">
          <SectionHeading>Now</SectionHeading>
          <CurrentCard entry={timeline[currentIdx]!} />
        </section>
      )}

      {earlier.length > 0 && (
        <section className="flex flex-col gap-2">
          <SectionHeading>Earlier this run</SectionHeading>
          <div className="flex flex-col gap-2">
            {earlier.map((entry) => (
              <EarlierCard key={entry.execId} entry={entry} />
            ))}
          </div>
        </section>
      )}

      {widgets.length > 0 && (
        <Suspense fallback={null}>
          <SessionWidgets widgets={widgets} />
        </Suspense>
      )}
    </div>
  );
}

// useSessionBoardWidgets fetches the LLM-curated widget spec for the run
// and refetches (debounced) as the run advances. Returns [] until/unless
// curation has produced widgets — the deterministic task board above does
// not depend on it. Gated on server_info.session_board_enabled: when the
// curation layer is off (the default), this never fetches, so most runs
// pay nothing. When on, it fetches immediately on run change and debounces
// refetches as the event stream advances.
function useSessionBoardWidgets(runId: string, lastSeq: number) {
  const enabled = useServerInfoStore(
    (s) => s.info?.session_board_enabled ?? false,
  );
  const queryClient = useQueryClient();
  // Fetch failures are best-effort (the deterministic task board above
  // doesn't depend on curation), so the query error is deliberately
  // unread.
  const query = useQuery({
    queryKey: ["session-board", runId],
    queryFn: ({ signal }) => getSessionBoard(runId, { signal }),
    enabled: enabled && !!runId,
  });
  const fetchedRun = useRef<string | null>(null);

  // The first fetch for a run is the query's own mount fetch — immediate,
  // since a finished run has a stable seq and would otherwise wait out
  // the debounce. Subsequent seq advances invalidate on a 1.5s debounce
  // so a live run doesn't refetch every event.
  useEffect(() => {
    if (!runId || !enabled) return;
    if (fetchedRun.current !== runId) {
      fetchedRun.current = runId;
      return;
    }
    const t = window.setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: ["session-board", runId] });
    }, 1500);
    return () => window.clearTimeout(t);
  }, [runId, lastSeq, enabled, queryClient]);

  return query.data?.widgets ?? [];
}

function SectionHeading({ children }: { children: React.ReactNode }) {
  return (
    <h3 className="text-caption font-semibold uppercase tracking-wide text-fg-subtle">
      {children}
    </h3>
  );
}

// CurrentCard is the prominent "what's happening now" card: friendly node
// label, status, a progress bar, the in-progress item called out in
// natural language, then the full checklist.
function CurrentCard({ entry }: { entry: TodoTimelineEntry }) {
  const todos = entry.latest.todos;
  const counts = countByStatus(todos);
  const active = todos.find((t) => t.status === "in_progress");
  const running = entry.exec?.status === "running";

  return (
    <div className="rounded-lg border border-border-default bg-surface-1 p-3 flex flex-col gap-3">
      <div className="flex items-center gap-2">
        {running ? (
          <LiveDot tone="live" size="sm" label="running" />
        ) : (
          <span
            className="w-1.5 h-1.5 rounded-full bg-fg-subtle flex-none"
            aria-hidden
          />
        )}
        <span className="text-label font-semibold text-fg-default">
          {nodeLabel(entry)}
        </span>
        <StepBadge entry={entry} />
        <span className="ml-auto text-caption text-fg-subtle">
          {counts.completed} / {todos.length} done
        </span>
      </div>

      <ProgressBar done={counts.completed} total={todos.length} active={running} />

      {active && (
        <div className="flex items-start gap-2 rounded-md bg-surface-2 px-2.5 py-2">
          <span className="text-warning-fg mt-px flex-none" aria-hidden>
            {STATUS_GLYPH.in_progress}
          </span>
          <span className="text-fg-default">
            {active.activeForm ?? active.content}
          </span>
        </div>
      )}

      <TodoItems todos={todos} />
    </div>
  );
}

// EarlierCard is a collapsed-by-default summary of a prior node's final
// task list; expanding it reveals the checklist. Uses native <details>
// so it needs no extra state.
function EarlierCard({ entry }: { entry: TodoTimelineEntry }) {
  const todos = entry.latest.todos;
  const counts = countByStatus(todos);
  const allDone = counts.completed === todos.length && todos.length > 0;

  return (
    <details className="group rounded-lg border border-border-default bg-surface-1">
      <summary className="flex cursor-pointer items-center gap-2 px-3 py-2 list-none">
        <span
          className="text-fg-subtle transition-transform group-open:rotate-90 flex-none"
          aria-hidden
        >
          ▸
        </span>
        <span className="text-label font-medium text-fg-default">
          {nodeLabel(entry)}
        </span>
        <StepBadge entry={entry} />
        <span className="ml-auto">
          {allDone ? (
            <Badge variant="success" size="sm">
              all done
            </Badge>
          ) : (
            <span className="text-caption text-fg-subtle">
              {counts.completed} / {todos.length} done
            </span>
          )}
        </span>
      </summary>
      <div className="px-3 pb-3 pt-1 border-t border-border-subtle">
        <TodoItems todos={todos} />
      </div>
    </details>
  );
}

// StepBadge shows how many distinct task-list updates this node made, so
// a node that re-planned several times reads as a richer journey. Hidden
// when there was only one snapshot (the common case).
function StepBadge({ entry }: { entry: TodoTimelineEntry }) {
  const n = entry.snapshots.length;
  if (n <= 1) return null;
  return (
    <Badge variant="neutral" size="sm">
      {n} updates
    </Badge>
  );
}

function ProgressBar({
  done,
  total,
  active,
}: {
  done: number;
  total: number;
  active: boolean;
}) {
  const pct = total > 0 ? Math.round((done / total) * 100) : 0;
  return (
    <div
      className="h-1.5 w-full overflow-hidden rounded-full bg-surface-3"
      role="progressbar"
      aria-valuenow={pct}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div
        className={`h-full rounded-full transition-all ${
          active ? "bg-live" : "bg-success"
        }`}
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}

// nodeLabel turns the IR node id into friendly English ("review_claude" →
// "Review claude"). Falls back to a generic label when the exec hasn't
// been joined yet.
function nodeLabel(entry: TodoTimelineEntry): string {
  const nodeId = entry.exec?.ir_node_id ?? "";
  const iteration = entry.exec?.loop_iteration ?? 0;
  const base = nodeId ? humanizeKey(nodeId) : "Task list";
  return iteration > 0 ? `${base} · pass ${iteration + 1}` : base;
}

// pickCurrentIndex chooses the "Now" entry: the running execution with the
// most recent update, else the most recently updated entry overall. The
// timeline is sorted oldest→newest, so the last running entry (or the last
// entry) is the natural pick.
function pickCurrentIndex(timeline: TodoTimelineEntry[]): number {
  let runningIdx = -1;
  let runningTs = -Infinity;
  let latestIdx = 0;
  let latestTs = -Infinity;
  for (let i = 0; i < timeline.length; i++) {
    const e = timeline[i]!;
    if (e.latest.updatedAt > latestTs) {
      latestTs = e.latest.updatedAt;
      latestIdx = i;
    }
    if (e.exec?.status === "running" && e.latest.updatedAt >= runningTs) {
      runningTs = e.latest.updatedAt;
      runningIdx = i;
    }
  }
  return runningIdx >= 0 ? runningIdx : latestIdx;
}
