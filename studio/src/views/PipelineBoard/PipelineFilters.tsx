import { LabelFilter } from "@/components/shared/LabelFilterPopover";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

import { pipelineFiltersActive, type PipelineFilterState } from "./filters";

// PipelineFilters is the /pipelines counterpart of the /board filter bar:
// text search, bot select, label multi-select, a filtered/total counter and
// a reset — same look, same semantics (labels AND, exact bot).
export function PipelineFilters({
  filters,
  allBots,
  allLabels,
  total,
  filtered,
  onChange,
  onReset,
}: {
  filters: PipelineFilterState;
  allBots: string[];
  allLabels: string[];
  total: number;
  filtered: number;
  onChange: (next: PipelineFilterState) => void;
  onReset: () => void;
}) {
  const active = pipelineFiltersActive(filters);

  const toggleLabel = (label: string) => {
    const labels = new Set(filters.labels);
    if (labels.has(label)) labels.delete(label);
    else labels.add(label);
    onChange({ ...filters, labels });
  };

  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-border-default bg-surface-1 px-3 py-2 text-xs">
      <div className="min-w-[200px] flex-shrink-0">
        <Input
          type="search"
          value={filters.query}
          onChange={(e) => onChange({ ...filters, query: e.target.value })}
          placeholder="Search title / body / run id…"
          aria-label="Search pipelines"
        />
      </div>
      {allBots.length > 0 && (
        <div className="w-auto">
          <Select
            value={filters.bot}
            onChange={(e) => onChange({ ...filters, bot: e.target.value })}
            aria-label="Filter by bot"
          >
            <option value="">All bots</option>
            {allBots.map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </Select>
        </div>
      )}
      {allLabels.length > 0 && (
        <LabelFilter
          allLabels={allLabels}
          selected={filters.labels}
          onToggle={toggleLabel}
          onClear={() => onChange({ ...filters, labels: new Set() })}
        />
      )}
      <span className="ml-auto text-fg-muted">
        {active ? `${filtered} / ${total}` : `${total} card${total === 1 ? "" : "s"}`}
      </span>
      {active && (
        <Button variant="ghost" size="sm" onClick={onReset}>
          reset
        </Button>
      )}
    </div>
  );
}

export default PipelineFilters;
