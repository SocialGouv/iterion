import { BotFilterSelect } from "@/components/shared/BotFilterSelect";
import { LabelFilter } from "@/components/shared/LabelFilterPopover";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

import {
  INVENTORY_SORT_OPTIONS,
  sortModeForTab,
  withSortModeForTab,
  pipelineFiltersActive,
  type ClosedSubfilter,
  type DepsFilter,
  type InventorySortMode,
  type InventoryTab,
  type OpenedSubfilter,
  type PipelineFilterState,
} from "./filters";

// PipelineFilters sits above the inventory card grid (tabbed Opened | Closed):
// text search, bot select, label multi-select, a filtered/total counter and a
// reset (labels AND, exact bot). When `repoScope` is set (cloud + a chosen
// active repo) a companion "Include unscoped" checkbox mirrors /board's
// BoardFilters affordance, letting the operator surface cards that carry no
// repo identity alongside the scoped set.
export function PipelineFilters({
  filters,
  allBots,
  allLabels,
  allKinds = [],
  allFamilies = [],
  total,
  filtered,
  onChange,
  onReset,
  repoScope,
  includeUnscoped,
  onIncludeUnscopedChange,
  showInventoryChrome = false,
  tabCounts,
}: {
  filters: PipelineFilterState;
  allBots: string[];
  allLabels: string[];
  allKinds?: string[];
  allFamilies?: string[];
  total: number;
  filtered: number;
  onChange: (next: PipelineFilterState) => void;
  onReset: () => void;
  repoScope?: string | null;
  includeUnscoped?: boolean;
  onIncludeUnscopedChange?: (v: boolean) => void;
  /** Tabs Opened/Closed + subfilter chips. */
  showInventoryChrome?: boolean;
  tabCounts?: { opened: number; closed: number };
}) {
  const active = pipelineFiltersActive(filters);
  const tab: InventoryTab = filters.inventoryTab ?? "opened";
  // Each tab carries its own sort, so what the control shows is what the grid
  // does — and changing it here cannot re-order the other tab.
  const sortValue = sortModeForTab(filters, tab);

  const toggleLabel = (label: string) => {
    const labels = new Set(filters.labels);
    if (labels.has(label)) labels.delete(label);
    else labels.add(label);
    onChange({ ...filters, labels });
  };

  const setTab = (next: InventoryTab) => {
    if (next === tab) return;
    onChange({ ...filters, inventoryTab: next });
  };

  return (
    <div className="space-y-2 rounded-lg border border-border-default bg-surface-1 px-3 py-2 text-xs">
      {showInventoryChrome && (
        <div className="space-y-2">
          <div
            className="flex gap-0 border-b border-border-default"
            role="tablist"
            aria-label="Inventory section"
          >
            {(
              [
                {
                  id: "opened" as const,
                  label: "Opened",
                  count: tabCounts?.opened,
                  title: "Queue and drafts — not finished yet",
                },
                {
                  id: "closed" as const,
                  label: "Closed",
                  count: tabCounts?.closed,
                  title: "Finished pipelines — success or failed",
                },
              ] as const
            ).map((t) => {
              const selected = tab === t.id;
              return (
                <button
                  key={t.id}
                  type="button"
                  role="tab"
                  aria-selected={selected}
                  title={t.title}
                  onClick={() => setTab(t.id)}
                  className={`-mb-px flex items-center gap-1.5 border-b-2 px-3 py-1.5 text-xs font-medium transition-colors ${
                    selected
                      ? "border-accent text-accent-text"
                      : "border-transparent text-fg-muted hover:text-fg-default"
                  }`}
                >
                  {t.label}
                  {typeof t.count === "number" && (
                    <span
                      className={`rounded-full px-1.5 py-0 text-micro tabular-nums ${
                        selected
                          ? "bg-accent-soft text-accent-text"
                          : "bg-surface-2 text-fg-subtle"
                      }`}
                    >
                      {t.count}
                    </span>
                  )}
                </button>
              );
            })}
          </div>

          {tab === "opened" ? (
            // Two INDEPENDENT axes, so they get separate labelled groups.
            // "Ready" and "Unblocked" read as synonyms side by side but are
            // not: a ticket can be staged Ready and still be parked on an
            // open dependency — that is exactly what "Mark ready (park if
            // deps open)" produces.
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
              <div className="flex items-center gap-1.5">
                <span className="text-micro text-fg-subtle">Readiness</span>
                <SubfilterChips
                  ariaLabel="Filter opened tickets by readiness"
                  value={filters.openedSubfilter ?? "all"}
                  options={[
                    { value: "all", label: "All", title: "Every opened ticket" },
                    {
                      value: "ready",
                      label: "Ready",
                      title: "Cleared to launch when a slot frees",
                    },
                    {
                      value: "not_ready",
                      label: "Not ready",
                      title: "Drafts and tickets blocked by deps",
                    },
                  ]}
                  onChange={(openedSubfilter: OpenedSubfilter) =>
                    onChange({ ...filters, openedSubfilter })
                  }
                />
              </div>
              <div className="flex items-center gap-1.5">
                <span className="text-micro text-fg-subtle">Dependencies</span>
                <SubfilterChips
                  ariaLabel="Filter opened tickets by dependency state"
                  value={filters.depsFilter ?? "all"}
                  options={[
                    { value: "all", label: "All", title: "No dependency filter" },
                    {
                      value: "unblocked",
                      label: "Unblocked",
                      title: "Nothing stands in the way — these can launch",
                    },
                    {
                      value: "blocked",
                      label: "Blocked",
                      title:
                        "Waiting on an open blocker, a required label, or parked in waiting_deps",
                    },
                  ]}
                  onChange={(depsFilter: DepsFilter) =>
                    onChange({ ...filters, depsFilter })
                  }
                />
              </div>
            </div>
          ) : (
            <SubfilterChips
              ariaLabel="Filter closed tickets"
              value={filters.closedSubfilter ?? "all"}
              options={[
                { value: "all", label: "All", title: "Every closed pipeline" },
                {
                  value: "success",
                  label: "Success",
                  title: "Finished successfully",
                },
                {
                  // Mid-flight failures live in the Needs attention lane now,
                  // so a "failed" card that reached Closed was stopped on
                  // purpose — cancelled, or filed via Close.
                  value: "failed",
                  label: "Stopped",
                  title: "Cancelled by the operator, or closed without finishing",
                },
              ]}
              onChange={(closedSubfilter: ClosedSubfilter) =>
                onChange({ ...filters, closedSubfilter })
              }
            />
          )}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <div className="min-w-[200px] flex-shrink-0">
          <Input
            type="search"
            value={filters.query}
            onChange={(e) => onChange({ ...filters, query: e.target.value })}
            placeholder="Search title / body / input_path / run id…"
            aria-label="Search pipelines"
          />
        </div>
        {allBots.length > 0 && (
          <BotFilterSelect
            value={filters.bot}
            allBots={allBots}
            onChange={(bot) => onChange({ ...filters, bot })}
            ariaLabel="Filter by bot"
          />
        )}
        {allLabels.length > 0 && (
          <LabelFilter
            allLabels={allLabels}
            selected={filters.labels}
            onToggle={toggleLabel}
            onClear={() => onChange({ ...filters, labels: new Set() })}
            label="Tags"
            searchPlaceholder="Search tags…"
          />
        )}
        {repoScope && onIncludeUnscopedChange && (
          <Checkbox
            checked={!!includeUnscoped}
            onChange={(e) => onIncludeUnscopedChange(e.target.checked)}
            label={
              !includeUnscoped && total > filtered
                ? `Include unscoped (${total - filtered} hidden)`
                : "Include unscoped"
            }
            help={`When on, cards with no repository also show alongside cards linked to ${repoScope}.`}
          />
        )}
        {allKinds.length > 0 && (
          <Select
            fit
            value={filters.pipelineKind}
            onChange={(e) => onChange({ ...filters, pipelineKind: e.target.value })}
            aria-label="Filter by pipeline kind"
            title="bot_args.pipeline_kind"
          >
            <option value="">All kinds</option>
            {allKinds.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </Select>
        )}
        {allFamilies.length > 0 && (
          <Select
            fit
            className="max-w-[10rem]"
            value={filters.familyId}
            onChange={(e) => onChange({ ...filters, familyId: e.target.value })}
            aria-label="Filter by family id"
            title="bot_args.family_id"
          >
            <option value="">All families</option>
            {allFamilies.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </Select>
        )}
        <label
          htmlFor="pipeline-inventory-sort"
          className="flex items-center gap-1.5 text-fg-muted"
          title="Order of cards in the inventory grid"
        >
          Sort
          <Select
            fit
            id="pipeline-inventory-sort"
            value={sortValue}
            onChange={(e) =>
              onChange(
                withSortModeForTab(
                  filters,
                  tab,
                  e.target.value as InventorySortMode,
                ),
              )
            }
            aria-label={`Sort ${tab} inventory cards`}
            title={
              sortValue !== "priority"
                ? "Order inventory cards — this tab remembers its own choice"
                : tab === "closed"
                  ? // Priority stays selectable here (which high-P pipelines
                    // completed?), but nothing in Closed is queued for launch,
                    // so promising "the launch queue's order" would be a lie.
                    "Higher priority first; ties oldest-first"
                  : "Higher priority first — same order as the launch queue; ties oldest-first"
            }
          >
            {INVENTORY_SORT_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </Select>
        </label>
        <span className="ml-auto text-fg-muted">
          {active || filtered !== total
            ? `${filtered} / ${total}`
            : `${total} card${total === 1 ? "" : "s"}`}
        </span>
        {active && (
          <Button variant="ghost" size="sm" onClick={onReset}>
            reset
          </Button>
        )}
      </div>
    </div>
  );
}

function SubfilterChips<T extends string>({
  ariaLabel,
  value,
  options,
  onChange,
}: {
  ariaLabel: string;
  value: T;
  options: { value: T; label: string; title: string }[];
  onChange: (next: T) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-1" role="group" aria-label={ariaLabel}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          title={o.title}
          onClick={() => onChange(o.value)}
          className={`rounded-full border px-2 py-0.5 text-micro font-medium transition-colors ${
            value === o.value
              ? "border-accent bg-accent-soft text-accent-text"
              : "border-border-default bg-surface-2 text-fg-muted hover:text-fg-default"
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export default PipelineFilters;
