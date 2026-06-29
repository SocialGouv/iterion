import { useEffect, useState } from "react";
import { useShallow } from "zustand/react/shallow";

import {
  selectActiveTodos,
  selectPendingAgents,
  useRunStore,
} from "@/store/run";
import type { TodoItem } from "@/components/Runs/toolFormatters";
import { TodoItems } from "@/components/Runs/todoChecklist";
import { countByStatus } from "@/components/Runs/todoStatus";

// LogSidePanel renders alongside the log stream and surfaces two
// concurrent signals that the footer can't carry without crowding it:
//   1. the live TodoWrite/todo_write task list scoped to the current
//      filter — visible "while a task list is in progress" so the
//      operator can see what the agent is working on without scrolling
//      the log;
//   2. a count of pending agentic tool calls (Agent/Task/agent/task)
//      with the oldest-elapsed time, because those don't show in the
//      footer (the random-words loader stays for them by design).
//
// Both signals fall back gracefully: when there's nothing to show in
// either, the parent collapses the column away.
export function LogSidePanel({
  filterNodeId,
  filterIteration,
}: {
  filterNodeId: string | null;
  filterIteration: number | null;
}) {
  const todos = useRunStore((s) =>
    selectActiveTodos(s, filterNodeId, filterIteration),
  );
  const pendingAgents = useRunStore(
    useShallow((s) => selectPendingAgents(s, filterNodeId, filterIteration)),
  );

  return (
    <div className="h-full flex flex-col gap-2 px-2 py-2 text-micro min-h-0 overflow-hidden">
      <AgentsBadge pending={pendingAgents} />
      <TodoChecklist todos={todos?.todos ?? null} source={todos?.source ?? null} />
    </div>
  );
}

function AgentsBadge({
  pending,
}: {
  pending: { toolName: string; startedAt: number }[];
}) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (pending.length === 0) return;
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [pending.length]);
  if (pending.length === 0) {
    return (
      <div className="text-fg-subtle italic text-caption px-1">
        No pending agents.
      </div>
    );
  }
  const oldest = pending[0]!;
  const elapsed = formatElapsed(Math.max(0, now - oldest.startedAt));
  const label =
    pending.length === 1
      ? `1 agent pending`
      : `${pending.length} agents pending`;
  return (
    <div className="flex items-center gap-2 px-2 py-1 rounded border border-border-default bg-surface-2 text-info-fg">
      <span aria-hidden="true">⚡</span>
      <span className="font-medium">{label}</span>
      <span className="ml-auto text-fg-subtle not-italic font-mono">
        {elapsed}
      </span>
    </div>
  );
}

function TodoChecklist({
  todos,
  source,
}: {
  todos: TodoItem[] | null;
  source: string | null;
}) {
  if (!todos || todos.length === 0) {
    return (
      <div className="flex-1 min-h-0 flex items-start">
        <div className="text-fg-subtle italic text-caption px-1">
          No active task list.
        </div>
      </div>
    );
  }
  const counts = countByStatus(todos);
  return (
    <div className="flex-1 min-h-0 flex flex-col gap-1 overflow-hidden">
      <div className="flex items-baseline gap-2 px-1">
        <span className="font-medium text-fg-default">Task list</span>
        <span className="text-fg-subtle text-caption">
          {todos.length} · {counts.in_progress} in progress · {counts.completed}{" "}
          done
        </span>
        {source && (
          <span className="ml-auto text-fg-subtle text-caption font-mono">
            {source}
          </span>
        )}
      </div>
      <div className="flex-1 min-h-0 overflow-y-auto pr-1">
        <TodoItems todos={todos} />
      </div>
    </div>
  );
}

function formatElapsed(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.floor(s % 60);
  return `${m}m${rem.toString().padStart(2, "0")}s`;
}
