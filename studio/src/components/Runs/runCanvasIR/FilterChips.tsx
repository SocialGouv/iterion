import type { StatusFilter } from "./helpers";

// Three chips (Failed / Running / Paused) the user toggles to dim
// non-matching nodes in the canvas. Each chip carries a tone class
// (text + border) and the count of executions in that status across
// the entire IR. Chips with `count === 0` are filtered out so the
// toolbar stays small on healthy runs.
export interface FilterChip {
  key: StatusFilter;
  label: string;
  count: number;
  tone: string;
}

export function buildFilterChips(counts: {
  failed: number;
  running: number;
  paused: number;
}): FilterChip[] {
  return [
    {
      key: "failed",
      label: "Failed",
      count: counts.failed,
      tone: "text-danger-fg border-danger/40",
    },
    {
      key: "running",
      label: "Running",
      count: counts.running,
      tone: "text-info-fg border-info/40",
    },
    {
      key: "paused",
      label: "Paused",
      count: counts.paused,
      tone: "text-warning-fg border-warning/40",
    },
  ];
}

export function FilterChips({
  chips,
  activeFilters,
  onToggle,
}: {
  chips: FilterChip[];
  activeFilters: Set<StatusFilter>;
  onToggle: (key: StatusFilter) => void;
}) {
  return (
    <>
      {chips
        .filter((c) => c.count > 0)
        .map((c) => {
          const isActive = activeFilters.has(c.key);
          return (
            <button
              key={c.key}
              type="button"
              onClick={() => onToggle(c.key)}
              aria-pressed={isActive}
              className={`text-caption px-2 py-0.5 rounded border transition-colors bg-surface-1/90 backdrop-blur ${
                c.tone
              } ${
                isActive
                  ? "ring-1 ring-accent bg-surface-2"
                  : "hover:bg-surface-2"
              }`}
              title={
                isActive
                  ? `Stop highlighting ${c.label.toLowerCase()} nodes`
                  : `Highlight ${c.label.toLowerCase()} nodes`
              }
            >
              {c.label} <span className="font-mono">{c.count}</span>
            </button>
          );
        })}
    </>
  );
}
