import { useEffect, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

import {
  BASE_GROUP_OPTIONS,
  groupModeForField,
  SORT_OPTIONS,
  type GroupMode,
  type SortMode,
} from "./boardShared";

export function BoardFilters({
  searchQuery,
  labelFilter,
  assigneeFilter,
  allLabels,
  allAssignees,
  total,
  filtered,
  onSearchChange,
  onLabelToggle,
  onClearLabels,
  onAssigneeChange,
  sortMode,
  onSortChange,
  groupMode,
  onGroupChange,
  fieldNames,
  onReset,
}: {
  searchQuery: string;
  labelFilter: Set<string>;
  assigneeFilter: string;
  allLabels: string[];
  allAssignees: string[];
  total: number;
  filtered: number;
  onSearchChange: (v: string) => void;
  onLabelToggle: (l: string) => void;
  onClearLabels: () => void;
  onAssigneeChange: (v: string) => void;
  sortMode: SortMode;
  onSortChange: (m: SortMode) => void;
  groupMode: GroupMode;
  onGroupChange: (m: GroupMode) => void;
  fieldNames: string[];
  onReset: () => void;
}) {
  const filtersActive =
    searchQuery.trim() !== "" || labelFilter.size > 0 || assigneeFilter !== "";
  return (
    <div className="px-3 py-2 border-b border-border-default bg-surface-1 flex flex-wrap items-center gap-2 text-xs">
      <div className="min-w-[200px] flex-shrink-0">
        <Input
          type="search"
          value={searchQuery}
          onChange={(e) => onSearchChange(e.target.value)}
          placeholder="Search title / body / id…"
          aria-label="Search issues"
        />
      </div>
      {allAssignees.length > 0 && (
        <div className="w-auto">
          <Select
            value={assigneeFilter}
            onChange={(e) => onAssigneeChange(e.target.value)}
            aria-label="Filter by assignee"
          >
            <option value="">All assignees</option>
            {allAssignees.map((a) => (
              <option key={a} value={a}>
                @{a}
              </option>
            ))}
          </Select>
        </div>
      )}
      {allLabels.length > 0 && (
        <LabelFilter
          allLabels={allLabels}
          selected={labelFilter}
          onToggle={onLabelToggle}
          onClear={onClearLabels}
        />
      )}
      <label htmlFor="board-sort-select" className="flex items-center gap-1 text-fg-muted">
        Sort
        <Select
          id="board-sort-select"
          value={sortMode}
          onChange={(e) => onSortChange(e.target.value as SortMode)}
          title="Order cards within each column"
        >
          {SORT_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
      </label>
      <label htmlFor="board-group-select" className="flex items-center gap-1 text-fg-muted">
        Group
        <Select
          id="board-group-select"
          value={groupMode}
          onChange={(e) => onGroupChange(e.target.value)}
          title="Split the board into horizontal swimlanes"
        >
          {BASE_GROUP_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
          {fieldNames.map((n) => (
            <option key={groupModeForField(n)} value={groupModeForField(n)}>
              Field: {n}
            </option>
          ))}
        </Select>
      </label>
      <span className="ml-auto text-fg-muted">
        {filtersActive ? `${filtered} / ${total}` : `${total} issue${total === 1 ? "" : "s"}`}
      </span>
      {filtersActive && (
        <Button variant="ghost" size="sm" onClick={onReset}>
          reset
        </Button>
      )}
    </div>
  );
}

// LabelFilter is a searchable multi-select popover for the board's label
// vocabulary. It replaces the flat chip strip, which grew unwieldy once
// boards accumulate dozens of labels. Selection is the same `labelFilter`
// Set the card chips toggle, so the two stay in sync.
function LabelFilter({
  allLabels,
  selected,
  onToggle,
  onClear,
}: {
  allLabels: string[];
  selected: Set<string>;
  onToggle: (l: string) => void;
  onClear: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const rootRef = useRef<HTMLDivElement | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
        setQuery("");
      }
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? allLabels.filter((l) => l.toLowerCase().includes(q)) : allLabels;
  }, [allLabels, query]);

  const count = selected.size;

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => {
          setOpen((o) => !o);
          setTimeout(() => inputRef.current?.focus(), 0);
        }}
        className={`px-2 py-1 rounded border flex items-center gap-1 ${
          count > 0
            ? "border-accent text-fg-default bg-accent-soft/30"
            : "border-border-default text-fg-muted hover:text-fg-default bg-surface-0"
        }`}
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span>Labels</span>
        {count > 0 && (
          <span className="px-1 rounded bg-accent text-fg-onAccent text-caption">{count}</span>
        )}
        <span className="text-fg-subtle text-caption">▾</span>
      </button>

      {open && (
        <div className="absolute z-[var(--z-popover)] mt-1 w-64 max-h-80 overflow-hidden rounded-md border border-border-strong bg-surface-0 shadow-popover flex flex-col">
          <div className="p-1 border-b border-border-default shrink-0">
            <Input
              ref={inputRef}
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search labels…"
              aria-label="Search labels"
            />
          </div>
          <ul className="py-1 overflow-auto">
            {filtered.length === 0 && (
              <li className="px-2 py-2 text-xs text-fg-subtle italic">No matches</li>
            )}
            {filtered.map((l) => {
              const active = selected.has(l);
              return (
                <li key={l}>
                  <button
                    type="button"
                    onClick={() => onToggle(l)}
                    className={`w-full text-left px-2 py-1.5 text-xs flex items-center gap-2 hover:bg-surface-1 rounded focus:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
                      active ? "text-fg-default" : "text-fg-muted"
                    }`}
                  >
                    <span
                      className={`inline-flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded border text-[9px] ${
                        active
                          ? "bg-accent border-accent text-fg-onAccent"
                          : "border-border-strong"
                      }`}
                    >
                      {active ? "✓" : ""}
                    </span>
                    <span className="truncate">{l}</span>
                  </button>
                </li>
              );
            })}
          </ul>
          {count > 0 && (
            <div className="p-1 border-t border-border-default shrink-0">
              <Button
                variant="ghost"
                size="sm"
                onClick={onClear}
                className="w-full justify-center"
              >
                Clear {count} label{count > 1 ? "s" : ""}
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
