import type { TodoItem } from "@/components/Runs/toolFormatters";

// Shared status vocabulary + counting for the agent task list
// (TodoWrite / todo_write). Kept separate from the TodoItems component
// (todoChecklist.tsx) so this module exports only constants/functions —
// mixing them with a component trips react-refresh/only-export-components.

export const STATUS_GLYPH: Record<TodoItem["status"], string> = {
  pending: "○",
  in_progress: "◐",
  completed: "●",
};

export const STATUS_COLOR: Record<TodoItem["status"], string> = {
  pending: "text-fg-subtle",
  in_progress: "text-warning-fg",
  completed: "text-success-fg",
};

export interface TodoCounts {
  pending: number;
  in_progress: number;
  completed: number;
}

export function countByStatus(todos: TodoItem[]): TodoCounts {
  let pending = 0;
  let inProgress = 0;
  let completed = 0;
  for (const t of todos) {
    if (t.status === "in_progress") inProgress++;
    else if (t.status === "completed") completed++;
    else pending++;
  }
  return { pending, in_progress: inProgress, completed };
}
