// Shared symbols used by both BoardView (index.tsx) and the leaf
// components extracted into sibling files (Column.tsx, SelectionToolbar.tsx,
// …). Kept narrow on purpose: only symbols that would otherwise force a
// circular import live here.

// Custom MIME for drag payloads carrying one or more issue ids
// (JSON-encoded `string[]`). Matches the studio's existing
// `application/iterion-*` convention so external drops can't
// accidentally be interpreted as text/plain.
export const DRAG_MIME_ISSUE_IDS = "application/iterion-issue-ids";

// Custom MIME for column-reorder drag payloads carrying the dragged
// state name. A column's onDrop checks this FIRST so a header drag
// reorders columns instead of being mistaken for a card drop.
export const DRAG_MIME_STATE = "application/iterion-state-name";

// BOARD_PALETTE is the set of column colors offered by the column-edit
// dialog's swatch picker. Values are CSS vars so they track the active
// theme (light/dark) — the same vars defaultStateColor() returns.
export const BOARD_PALETTE: { label: string; value: string }[] = [
  { label: "Backlog", value: "var(--color-board-backlog)" },
  { label: "Ready", value: "var(--color-board-ready)" },
  { label: "In progress", value: "var(--color-board-in-progress)" },
  { label: "Review", value: "var(--color-board-review)" },
  { label: "Done", value: "var(--color-board-done)" },
  { label: "Blocked", value: "var(--color-board-blocked)" },
];

// Priority presets offered by the bulk "Priority" picker (the magnitudes
// the roadmap uses). Columns sort by priority descending by default.
export const PRIORITY_PRESETS = [0, 1, 2, 3, 5, 10, 20, 30];

// Intra-column ordering modes offered by the board's Sort selector.
export type SortMode = "priority" | "updated" | "created" | "title";

export const SORT_OPTIONS: { value: SortMode; label: string }[] = [
  { value: "priority", label: "Priority" },
  { value: "updated", label: "Recently updated" },
  { value: "created", label: "Recently created" },
  { value: "title", label: "Title (A–Z)" },
];

// defaultStateColor maps the conventional native-tracker state names
// (backlog/ready/in_progress/review/done/blocked) to a sensible palette
// so columns are scannable out of the box. Custom states fall back to a
// semantic colour from the eligible/terminal flags; truly unknown states
// get a neutral slate. Custom boards can always override per-state via
// the `color:` field — this helper only fires when `State.Color` is
// empty.
export function defaultStateColor(name: string, eligible: boolean, terminal: boolean): string {
  switch (name) {
    case "backlog":
      return "var(--color-board-backlog)";
    case "ready":
      return "var(--color-board-ready)";
    case "in_progress":
      return "var(--color-board-in-progress)";
    case "review":
      return "var(--color-board-review)";
    case "done":
      return "var(--color-board-done)";
    case "blocked":
      return "var(--color-board-blocked)";
    default:
      if (terminal) return "var(--color-board-done)";
      if (eligible) return "var(--color-board-ready)";
      return "var(--color-board-backlog)";
  }
}
