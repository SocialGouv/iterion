import { useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { listPlans } from "@/api/runs";
import type { PlanSnapshot } from "@/api/runs/types";
import { formatTime } from "@/lib/format";
import { humanizeKey } from "@/lib/humanizeKey";
import { useRunStore } from "@/store/run";

import { TodoItems } from "./todoChecklist";
import type { TodoItem, TodoStatus } from "./toolFormatters";

// Events after which a new plan snapshot may have been captured. A plan is
// written on a TodoWrite/todo_write tool call (tool_started), the run
// engine emits a dedicated plan_written event on each new snapshot, and we
// also catch the terminal triplet so the final plan lands without a manual
// refresh. Mirrors ArtifactFilesPanel's refresh discipline.
const REFRESH_EVENTS = new Set([
  "plan_written",
  "tool_started",
  "node_finished",
  "run_finished",
  "run_failed",
  "run_cancelled",
]);

const DEBOUNCE_MS = 300;
const VALID_STATUS: ReadonlySet<string> = new Set([
  "pending",
  "in_progress",
  "completed",
]);

// PlansPanel surfaces every persisted plan snapshot of a run — the living
// TODO lists agents (esp. the campaign agent in the improve-loop bots)
// maintain via TodoWrite/todo_write, captured to runs/<id>/plans/. Ordered
// newest-first so the current plan is at the top, with the earlier passes
// below it: you see the plan AND how it evolved across the run's nodes and
// loop iterations. Reuses TodoItems (the same checklist the live Tasks /
// Logs panels render) so a snapshot looks identical to the live view.
export default function PlansPanel({ runId }: { runId: string | null }) {
  const plans = useRunPlans(runId);

  if (!runId) {
    return (
      <div className="h-full flex items-center justify-center text-fg-subtle text-xs px-4">
        No active run.
      </div>
    );
  }

  if (plans === null) {
    return (
      <div className="h-full flex items-center justify-center text-fg-subtle text-xs px-4">
        Loading plans…
      </div>
    );
  }

  if (plans.length === 0) {
    return (
      <div className="h-full flex flex-col items-center justify-center text-fg-subtle px-4 text-center gap-1 text-xs">
        <div>No plans captured for this run.</div>
        <div className="opacity-70">
          Agents that maintain a task list (via <code>TodoWrite</code> /{" "}
          <code>todo_write</code>) have each plan snapshot recorded here.
        </div>
      </div>
    );
  }

  // Newest-first: the latest plan reads at the top, earlier passes below.
  const ordered = [...plans].sort((a, b) => b.seq - a.seq);
  return (
    <div className="h-full flex flex-col text-xs">
      <div className="flex items-center justify-between border-b border-border-subtle px-3 py-1.5 text-fg-subtle">
        <span>
          {plans.length} plan{plans.length === 1 ? "" : "s"}
        </span>
        <span className="opacity-70">newest first</span>
      </div>
      <div className="flex-1 overflow-auto px-3 py-3 flex flex-col gap-3">
        {ordered.map((p) => (
          <PlanCard key={p.seq} plan={p} />
        ))}
      </div>
    </div>
  );
}

function PlanCard({ plan }: { plan: PlanSnapshot }) {
  const todos = toTodoItems(plan);
  const done = todos.filter((t) => t.status === "completed").length;
  return (
    <div className="rounded-lg border border-border-default bg-surface-1">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 px-3 py-2 border-b border-border-subtle">
        <span className="text-label font-medium text-fg-default">
          {humanizeKey(plan.node_id)}
        </span>
        {plan.iteration > 0 && (
          <span className="text-caption text-fg-subtle">
            pass {plan.iteration}
          </span>
        )}
        <span className="ml-auto text-caption text-fg-subtle whitespace-nowrap">
          {done}/{todos.length} · {formatTime(plan.ts)}
        </span>
      </div>
      <div className="px-2 py-2">
        {todos.length === 0 ? (
          <div className="text-fg-subtle px-1">Empty plan.</div>
        ) : (
          <TodoItems todos={todos} />
        )}
      </div>
    </div>
  );
}

// toTodoItems maps a wire PlanSnapshot's todos onto the shared TodoItem
// shape TodoItems renders (active_form → activeForm; status guarded to the
// known set, defaulting to pending for any unexpected value).
function toTodoItems(plan: PlanSnapshot): TodoItem[] {
  return plan.todos.map((t) => ({
    content: t.content,
    status: (VALID_STATUS.has(t.status) ? t.status : "pending") as TodoStatus,
    activeForm: t.active_form,
  }));
}

// useRunPlans fetches the plan list once per run, then invalidates
// (debounced) when a plan-producing event arrives. Returns null until the
// first load resolves, then the (possibly empty) list. Load failures are
// best-effort (the panel just keeps its loading state). Mirrors
// ArtifactFilesPanel's refresh discipline.
function useRunPlans(runId: string | null): PlanSnapshot[] | null {
  const queryClient = useQueryClient();
  const events = useRunStore((s) => s.events);
  const lastSeenSeq = useRef<number>(-1);
  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null);

  const query = useQuery({
    queryKey: ["run-plans", runId],
    queryFn: ({ signal }) => listPlans(runId!, { signal }),
    enabled: !!runId,
  });

  // Reset the seq high-water mark on run change.
  useEffect(() => {
    lastSeenSeq.current = -1;
  }, [runId]);

  // Debounced refetch when a new plan-producing event lands.
  useEffect(() => {
    if (!runId || events.length === 0) return;
    let touched = false;
    for (const ev of events) {
      if (ev.seq <= lastSeenSeq.current) continue;
      lastSeenSeq.current = ev.seq;
      if (REFRESH_EVENTS.has(ev.type)) touched = true;
    }
    if (!touched) return;
    if (debounce.current) clearTimeout(debounce.current);
    debounce.current = setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: ["run-plans", runId] });
    }, DEBOUNCE_MS);
    return () => {
      if (debounce.current) clearTimeout(debounce.current);
    };
  }, [events, runId, queryClient]);

  return runId ? query.data ?? null : null;
}
