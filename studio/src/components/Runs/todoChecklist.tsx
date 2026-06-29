import type { TodoItem } from "@/components/Runs/toolFormatters";
import { STATUS_COLOR, STATUS_GLYPH } from "@/components/Runs/todoStatus";

// TodoItems renders the agent task-list body (no header): one row per
// item with a status glyph and its text — the in_progress item shows its
// natural-language activeForm, completed items strike through. The caller
// owns scroll/overflow and any header. Shared by the live Logs side panel
// (LogSidePanel) and the persistent Session board (SessionBoardTab) so the
// two surfaces render the checklist identically.
export function TodoItems({ todos }: { todos: TodoItem[] }) {
  return (
    <ul className="flex flex-col gap-0.5">
      {todos.map((t, idx) => {
        const text =
          t.status === "in_progress" ? t.activeForm ?? t.content : t.content;
        return (
          <li
            key={idx}
            className="flex items-start gap-1.5 px-1 py-0.5 leading-snug"
          >
            <span
              className={`${STATUS_COLOR[t.status]} flex-none mt-px`}
              aria-label={t.status}
            >
              {STATUS_GLYPH[t.status]}
            </span>
            <span
              className={
                t.status === "completed"
                  ? "text-fg-subtle line-through"
                  : t.status === "in_progress"
                    ? "text-fg-default font-medium"
                    : "text-fg-default"
              }
            >
              {text}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
